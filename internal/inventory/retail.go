package inventory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
)

const LowStockThreshold = 3

type RetailItem struct {
	ID         int64
	Name       string
	Category   string
	Spec       string
	PriceCents int
	CostCents  int
	Stock      int
	Active     bool
}

var RetailCategories = []string{"罐装", "瓶装", "小食", "周边", "其他"}

func (it RetailItem) Price() string    { return platform.PriceText(it.PriceCents) }
func (it RetailItem) Cost() string     { return platform.PriceText(it.CostCents) }
func (it RetailItem) ProfitCents() int { return it.PriceCents - it.CostCents }
func (it RetailItem) Profit() string   { return platform.MoneyText(it.ProfitCents()) }
func (it RetailItem) StockValue() int  { return it.Stock * it.CostCents }
func (it RetailItem) PriceInput() string {
	return strconv.FormatFloat(float64(it.PriceCents)/100, 'f', -1, 64)
}
func (it RetailItem) CostInput() string {
	return strconv.FormatFloat(float64(it.CostCents)/100, 'f', -1, 64)
}

// LowStockItem 表示库存变动后跌入低库存/售罄阈值的商品快照，供事务提交后发布预警事件。
type LowStockItem struct {
	ItemID int64
	Name   string
	Stock  int
	Active bool
}

// SaleLine 是收银台里的一行：确定了的商品 + 份数。
type SaleLine struct {
	Item RetailItem
	Qty  int
}

// RetailStats 是零售页顶部的几个数字。
type RetailStats struct {
	TodayRev   int
	TodayCost  int
	MonthRev   int
	MonthCost  int
	StockValue int
	LowCount   int
	SKUs       int
}

func (r RetailStats) TodayRevText() string    { return platform.PriceText(r.TodayRev) }
func (r RetailStats) TodayProfitText() string { return platform.MoneyText(r.TodayRev - r.TodayCost) }
func (r RetailStats) MonthRevText() string    { return platform.PriceText(r.MonthRev) }
func (r RetailStats) MonthProfitText() string { return platform.MoneyText(r.MonthRev - r.MonthCost) }
func (r RetailStats) StockValueText() string  { return platform.PriceText(r.StockValue) }

type StockMove struct {
	Delta     int
	Reason    string
	CreatedAt time.Time
}

func (m StockMove) When() string { return m.CreatedAt.Format("01-02 15:04") }
func (m StockMove) DeltaText() string {
	if m.Delta > 0 {
		return "+" + strconv.Itoa(m.Delta)
	}
	return strconv.Itoa(m.Delta)
}

func ListRetail(ctx context.Context, pool *pgxpool.Pool) ([]RetailItem, error) {
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items order by active desc, category, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetailItem
	for rows.Next() {
		var it RetailItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func GetRetail(ctx context.Context, pool *pgxpool.Pool, id int64) (RetailItem, error) {
	var it RetailItem
	err := pool.QueryRow(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where id=$1`, id).
		Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active)
	return it, err
}

func SaveRetail(ctx context.Context, pool *pgxpool.Pool, it RetailItem) (int64, *LowStockItem, error) {
	if strings.TrimSpace(it.Name) == "" {
		return 0, nil, platform.UserErr("商品名不能为空")
	}
	if it.Stock < 0 {
		return 0, nil, platform.UserErr("库存不能为负")
	}
	if it.PriceCents < 0 || it.CostCents < 0 {
		return 0, nil, platform.UserErr("售价和成本不能为负")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)

	if it.ID == 0 {
		var id int64
		newActive := it.Active && it.Stock > 0
		if err := tx.QueryRow(ctx, `insert into retail_items (name, category, spec, price_cents, cost_cents, stock, active)
			values ($1,$2,$3,$4,$5,$6, ($7 and $6 > 0)) returning id`,
			it.Name, it.Category, it.Spec, it.PriceCents, it.CostCents, it.Stock, it.Active).Scan(&id); err != nil {
			return 0, nil, err
		}
		if it.Stock > 0 {
			if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason) values ($1,$2,'入库')`,
				id, it.Stock); err != nil {
				return 0, nil, err
			}
			if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.RetailSubject(it.Name),
				fmt.Sprintf("初始库存 %d", it.Stock)); err != nil {
				return 0, nil, err
			}
			if it.Active {
				if err := platform.LogOperation(ctx, tx, platform.OpRetailOn, platform.RetailSubject(it.Name),
					fmt.Sprintf("上架 · 库存 %d", it.Stock)); err != nil {
					return 0, nil, err
				}
			}
		}
		var low *LowStockItem
		if it.Stock <= LowStockThreshold {
			low = &LowStockItem{ItemID: id, Name: it.Name, Stock: it.Stock, Active: newActive}
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, nil, err
		}
		return id, low, nil
	}

	// 锁住商品行再算库存差值，流水与库存不会打架；库存为 0 的商品不能保存在架。
	var old int
	var oldActive bool
	if err := tx.QueryRow(ctx, `select stock, active from retail_items where id=$1 for update`,
		it.ID).Scan(&old, &oldActive); err != nil {
		return 0, nil, err
	}
	newActive := it.Active && it.Stock > 0
	if _, err := tx.Exec(ctx, `update retail_items set name=$2, category=$3, spec=$4, price_cents=$5,
		cost_cents=$6, stock=$7, active=$8 where id=$1`,
		it.ID, it.Name, it.Category, it.Spec, it.PriceCents, it.CostCents, it.Stock, newActive); err != nil {
		return 0, nil, err
	}
	if diff := it.Stock - old; diff != 0 {
		reason := "调整"
		if diff > 0 {
			reason = "入库"
		}
		if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason) values ($1,$2,$3)`,
			it.ID, diff, reason); err != nil {
			return 0, nil, err
		}
		if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.RetailSubject(it.Name),
			fmt.Sprintf("%s %+d，现有 %d", reason, diff, it.Stock)); err != nil {
			return 0, nil, err
		}
	}
	if newActive != oldActive {
		kind, verb := platform.OpRetailOff, "下架"
		if newActive {
			kind, verb = platform.OpRetailOn, "上架"
		}
		if err := platform.LogOperation(ctx, tx, kind, platform.RetailSubject(it.Name),
			fmt.Sprintf("%s · 库存 %d", verb, it.Stock)); err != nil {
			return 0, nil, err
		}
	}
	var low *LowStockItem
	if it.Stock < old && it.Stock <= LowStockThreshold {
		low = &LowStockItem{ItemID: it.ID, Name: it.Name, Stock: it.Stock, Active: newActive}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return it.ID, low, nil
}

func DeleteRetail(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `delete from retail_items where id=$1`, id)
	return err
}

// ToggleRetail 切上下架；库存为 0 时不允许上架（卖光了就下架，补货再手动上架）。
func ToggleRetail(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var active bool
	var stock int
	var name string
	err = tx.QueryRow(ctx, `update retail_items set active = not active where id=$1 and (active or stock > 0)
		returning active, stock, name`, id).Scan(&active, &stock, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("库存为 0，先入库再上架")
	}
	if err != nil {
		return err
	}
	kind, verb := platform.OpRetailOff, "下架"
	if active {
		kind, verb = platform.OpRetailOn, "上架"
	}
	if err := platform.LogOperation(ctx, tx, kind, platform.RetailSubject(name),
		fmt.Sprintf("%s · 现有 %d", verb, stock)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SellRetail 是店员端的结账事务入口（兼容无多商品合单场景的直接调用）。
func SellRetail(ctx context.Context, pool *pgxpool.Pool, lines []SaleLine) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, _, err := SellRetailTx(ctx, tx, lines)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// SellRetailTx 在调用方的事务里结账：扣库存、写订单与明细、记库存流水，并返回需要预警的低库存条目。
// 售价用传入的单价（点餐单按下单时的价），成本取数据库当前值。
func SellRetailTx(ctx context.Context, tx pgx.Tx, lines []SaleLine) (int64, []LowStockItem, error) {
	if len(lines) == 0 {
		return 0, nil, platform.UserErr("这一单里没有商品")
	}
	type line struct {
		id               int64
		name             string
		price, cost, qty int
	}
	sold := make([]line, 0, len(lines))
	var low []LowStockItem
	var total, cost int
	for _, l := range lines {
		if l.Qty <= 0 {
			return 0, nil, platform.UserErr("数量要大于 0")
		}
		if l.Item.PriceCents < 0 {
			return 0, nil, platform.UserErr("售价不能为负")
		}
		var name string
		var dbCost, newStock int
		var newActive bool
		err := tx.QueryRow(ctx, `update retail_items
			set stock = stock - $2,
			    active = case when stock - $2 > 0 then active else false end
			where id=$1 and stock >= $2
			returning name, cost_cents, stock, active`, l.Item.ID, l.Qty).Scan(&name, &dbCost, &newStock, &newActive)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.UserErr("「" + l.Item.Name + "」库存不够了")
		}
		if err != nil {
			return 0, nil, err
		}
		if dbCost < 0 {
			return 0, nil, platform.UserErr("「" + name + "」成本不能为负")
		}
		sold = append(sold, line{id: l.Item.ID, name: name, price: l.Item.PriceCents, cost: dbCost, qty: l.Qty})
		total += l.Item.PriceCents * l.Qty
		cost += dbCost * l.Qty
		if newStock <= LowStockThreshold {
			low = append(low, LowStockItem{ItemID: l.Item.ID, Name: name, Stock: newStock, Active: newActive})
		}
	}

	var saleID int64
	if err := tx.QueryRow(ctx, `insert into retail_sales (total_cents, cost_cents) values ($1,$2) returning id`,
		total, cost).Scan(&saleID); err != nil {
		return 0, nil, err
	}
	for _, l := range sold {
		if _, err := tx.Exec(ctx, `insert into retail_sale_items (sale_id, item_id, name, qty, price_cents, cost_cents)
			values ($1,$2,$3,$4,$5,$6)`, saleID, l.id, l.name, l.qty, l.price, l.cost); err != nil {
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason, ref_id) values ($1,$2,'售出',$3)`,
			l.id, -l.qty, saleID); err != nil {
			return 0, nil, err
		}
	}
	return saleID, low, nil
}

// DeleteSale 撤销订单：库存加回去，订单删掉（明细级联），流水留痕。
// 先锁订单行，并发撤销只有抢到锁的那次退库存，重复调用是空操作。
func DeleteSale(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var saleID int64
	err = tx.QueryRow(ctx, `select id from retail_sales where id=$1 for update`, id).Scan(&saleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 已经撤过了
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select item_id, qty from retail_sale_items where sale_id=$1 and item_id is not null`, id)
	if err != nil {
		return err
	}
	type back struct {
		id  int64
		qty int
	}
	var list []back
	for rows.Next() {
		var b back
		if err := rows.Scan(&b.id, &b.qty); err != nil {
			rows.Close()
			return err
		}
		list = append(list, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range list {
		if _, err := tx.Exec(ctx, `update retail_items set stock = stock + $2 where id=$1`, b.id, b.qty); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason, ref_id) values ($1,$2,'撤销',$3)`,
			b.id, b.qty, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from retail_sales where id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetailStatsSummary 今日与本月零售营收/成本。
func RetailStatsSummary(ctx context.Context, pool *pgxpool.Pool) (RetailStats, error) {
	var st RetailStats
	err := pool.QueryRow(ctx, `
		select coalesce(sum(total_cents) filter (where created_at::date = current_date), 0)::int,
		       coalesce(sum(cost_cents)  filter (where created_at::date = current_date), 0)::int,
		       coalesce(sum(total_cents) filter (where created_at >= date_trunc('month', now())), 0)::int,
		       coalesce(sum(cost_cents)  filter (where created_at >= date_trunc('month', now())), 0)::int
		from retail_sales`).Scan(&st.TodayRev, &st.TodayCost, &st.MonthRev, &st.MonthCost)
	return st, err
}

// ListMoves 某个商品最近的库存流水。
func ListMoves(ctx context.Context, pool *pgxpool.Pool, itemID int64, limit int) ([]StockMove, error) {
	rows, err := pool.Query(ctx, `select delta, reason, created_at from retail_moves
		where item_id=$1 order by id desc limit $2`, itemID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StockMove
	for rows.Next() {
		var m StockMove
		if err := rows.Scan(&m.Delta, &m.Reason, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchRetail 列表筛选：关键词、分类、只要低库存、只要在架。
func SearchRetail(ctx context.Context, pool *pgxpool.Pool, q, category string, lowOnly, activeOnly bool) ([]RetailItem, error) {
	q = strings.TrimSpace(q)
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items
		where ($1 = '' or name ilike '%'||$1||'%' or category ilike '%'||$1||'%' or spec ilike '%'||$1||'%')
		  and ($2 = '' or category = $2)
		  and (not $3 or stock <= $5)
		  and (not $4 or active)
		order by active desc, category, name`, q, category, lowOnly, activeOnly, LowStockThreshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RetailItem
	for rows.Next() {
		var it RetailItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// AddStock 入库/调整：改库存并记一条流水。扣到负数就拒绝，流水的 delta 就是真实变动。
func AddStock(ctx context.Context, pool *pgxpool.Pool, id int64, delta int, reason string) (*LowStockItem, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var oldActive bool
	err = tx.QueryRow(ctx, `select active from retail_items where id=$1 for update`, id).Scan(&oldActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.UserErr("库存不够扣，先核对数量")
	}
	if err != nil {
		return nil, err
	}
	var stock int
	var active bool
	var name string
	err = tx.QueryRow(ctx, `update retail_items
		set stock = stock + $2,
		    active = case when stock + $2 > 0 then active else false end
		where id=$1 and stock + $2 >= 0
		returning stock, active, name`, id, delta).Scan(&stock, &active, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.UserErr("库存不够扣，先核对数量")
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason) values ($1,$2,$3)`, id, delta, reason); err != nil {
		return nil, err
	}
	if delta != 0 {
		if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.RetailSubject(name),
			fmt.Sprintf("%s %+d，现有 %d", reason, delta, stock)); err != nil {
			return nil, err
		}
	}
	// 扣到 0 时上面的 SQL 会自动下架；只有原本在架才记这条，避免把补货/普通调整写成下架。
	if oldActive && !active {
		if err := platform.LogOperation(ctx, tx, platform.OpRetailOff, platform.RetailSubject(name), "下架 · 售罄"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if delta < 0 && stock <= LowStockThreshold {
		return &LowStockItem{ItemID: id, Name: name, Stock: stock, Active: active}, nil
	}
	return nil, nil
}
