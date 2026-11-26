package inventory

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	return platform.MoneyInput(it.PriceCents)
}
func (it RetailItem) CostInput() string {
	return platform.MoneyInput(it.CostCents)
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

func (m StockMove) When() string { return platform.TimeText(&m.CreatedAt) }
func (m StockMove) DeltaText() string {
	if m.Delta > 0 {
		return "+" + strconv.Itoa(m.Delta)
	}
	return strconv.Itoa(m.Delta)
}

func ListRetail(ctx context.Context, pool *pgxpool.Pool) ([]RetailItem, error) {
	rows, err := pool.Query(ctx, `select id, name, category, spec, price_cents, cost_cents, stock, active
		from retail_items where archived_at is null order by active desc, category, name`)
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
		from retail_items where id=$1 and archived_at is null`, id).
		Scan(&it.ID, &it.Name, &it.Category, &it.Spec, &it.PriceCents, &it.CostCents, &it.Stock, &it.Active)
	return it, err
}

func SaveRetail(ctx context.Context, pool *pgxpool.Pool, it RetailItem) (int64, *LowStockItem, error) {
	return SaveRetailWithExpected(ctx, pool, it, nil)
}

func SaveRetailWithExpected(ctx context.Context, pool *pgxpool.Pool, it RetailItem, expectedStock *int) (int64, *LowStockItem, error) {
	it.Name = strings.TrimSpace(it.Name)
	it.Category = strings.TrimSpace(it.Category)
	it.Spec = strings.TrimSpace(it.Spec)
	if it.Name == "" {
		return 0, nil, platform.UserErr("商品名不能为空")
	}
	if len([]rune(it.Name)) > 120 {
		return 0, nil, platform.UserErr("商品名过长（最多 120 字）")
	}
	if len([]rune(it.Category)) > 50 {
		return 0, nil, platform.UserErr("分类名称过长（最多 50 字）")
	}
	if len([]rune(it.Spec)) > 80 {
		return 0, nil, platform.UserErr("规格过长（最多 80 字）")
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
		if newActive && it.Stock > 0 && it.Stock <= LowStockThreshold {
			low = &LowStockItem{ItemID: id, Name: it.Name, Stock: it.Stock, Active: newActive}
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, nil, err
		}
		return id, low, nil
	}

	// 锁住商品行再算库存差值，流水与库存不会打架；库存为 0 的商品不能保存在架。
	var old int
	var oldActive, archived bool
	err = tx.QueryRow(ctx, `select stock, active, archived_at is not null from retail_items where id=$1 for update`,
		it.ID).Scan(&old, &oldActive, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, platform.UserErr("商品不存在")
	}
	if err != nil {
		return 0, nil, err
	}
	if archived {
		return 0, nil, platform.UserErr("商品已停用删除，无法编辑")
	}
	finalStock := it.Stock
	if expectedStock != nil {
		if it.Stock == *expectedStock {
			// 未改动库存字段时保留最新并发库存，防止覆盖收银刚扣减的库存。
			finalStock = old
		} else if old != *expectedStock {
			return 0, nil, platform.UserErr(fmt.Sprintf("商品库存刚刚已变动（当前库存 %d），请刷新后再改", old))
		}
	}
	newActive := it.Active && finalStock > 0
	if _, err := tx.Exec(ctx, `update retail_items set name=$2, category=$3, spec=$4, price_cents=$5,
		cost_cents=$6, stock=$7, active=$8 where id=$1`,
		it.ID, it.Name, it.Category, it.Spec, it.PriceCents, it.CostCents, finalStock, newActive); err != nil {
		return 0, nil, err
	}
	if diff := finalStock - old; diff != 0 {
		reason := "调整"
		if diff > 0 {
			reason = "入库"
		}
		if _, err := tx.Exec(ctx, `insert into retail_moves (item_id, delta, reason) values ($1,$2,$3)`,
			it.ID, diff, reason); err != nil {
			return 0, nil, err
		}
		if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.RetailSubject(it.Name),
			fmt.Sprintf("%s %+d，现有 %d", reason, diff, finalStock)); err != nil {
			return 0, nil, err
		}
	}
	if newActive != oldActive {
		kind, verb := platform.OpRetailOff, "下架"
		if newActive {
			kind, verb = platform.OpRetailOn, "上架"
		}
		if err := platform.LogOperation(ctx, tx, kind, platform.RetailSubject(it.Name),
			fmt.Sprintf("%s · 库存 %d", verb, finalStock)); err != nil {
			return 0, nil, err
		}
	}
	var low *LowStockItem
	if (old > LowStockThreshold && finalStock <= LowStockThreshold) || (old > 0 && finalStock == 0) {
		low = &LowStockItem{ItemID: it.ID, Name: it.Name, Stock: finalStock, Active: newActive}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return it.ID, low, nil
}

// DeleteRetail 软删除零售商品：保留历史销售/库存流水审计，阻止尚有未结 POS 订单的商品删除。
func DeleteRetail(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var name string
	var active bool
	err = tx.QueryRow(ctx, `select name, active from retail_items where id=$1 and archived_at is null for update`, id).Scan(&name, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("商品不存在或已删除")
	}
	if err != nil {
		return err
	}
	var hasOpenOrder bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from order_items where kind='retail' and ref_id=$1 and paid_at is null)`, id).Scan(&hasOpenOrder); err != nil {
		return err
	}
	if hasOpenOrder {
		return platform.UserErr("该商品还有未结账 POS 订单占用，先清理或结清订单")
	}
	if _, err := tx.Exec(ctx, `update retail_items set active=false, archived_at=now() where id=$1`, id); err != nil {
		return err
	}
	if active {
		if err := platform.LogOperation(ctx, tx, platform.OpRetailOff, platform.RetailSubject(name), "删除下架"); err != nil {
			return err
		}
	}
	if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.RetailSubject(name), "删除归档商品"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ToggleRetail 切上下架；库存为 0 时不允许上架（卖光了就下架，补货再手动上架）。
func ToggleRetail(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var oldActive, archived bool
	var stock int
	var name string
	err = tx.QueryRow(ctx, `select active, stock, name, archived_at is not null from retail_items where id=$1 for update`, id).
		Scan(&oldActive, &stock, &name, &archived)
	if errors.Is(err, pgx.ErrNoRows) || archived {
		return platform.UserErr("商品不存在或已删除")
	}
	if err != nil {
		return err
	}
	if !oldActive && stock <= 0 {
		return platform.UserErr("库存为 0，先入库再上架")
	}
	newActive := !oldActive
	if _, err := tx.Exec(ctx, `update retail_items set active=$2 where id=$1`, id, newActive); err != nil {
		return err
	}
	kind, verb := platform.OpRetailOff, "下架"
	if newActive {
		kind, verb = platform.OpRetailOn, "上架"
	}
	if err := platform.LogOperation(ctx, tx, kind, platform.RetailSubject(name),
		fmt.Sprintf("%s · 现有 %d", verb, stock)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SellRetail 是独立事务版零售结账，仅供测试与内部补录使用；正式 POS 结账必须在
// pos.PayItemsWithGateway 的同一事务内调用 SellRetailTx，确保扣库存与 payments 记账原子提交。
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

// SellRetailTx 在调用方的事务里结账：按固定 ID 升序加锁、扣库存、写订单与明细、记库存流水，并返回跨阈值低库存预警。
// 售价用传入的单价（点餐单按下单时的价），成本取数据库当前值。
func SellRetailTx(ctx context.Context, tx pgx.Tx, lines []SaleLine) (int64, []LowStockItem, error) {
	if len(lines) == 0 {
		return 0, nil, platform.UserErr("这一单里没有商品")
	}
	needQty := make(map[int64]int, len(lines))
	reqName := make(map[int64]string, len(lines))
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		if l.Qty <= 0 {
			return 0, nil, platform.UserErr("数量要大于 0")
		}
		if l.Item.PriceCents < 0 {
			return 0, nil, platform.UserErr("售价不能为负")
		}
		if _, seen := needQty[l.Item.ID]; !seen {
			ids = append(ids, l.Item.ID)
			reqName[l.Item.ID] = l.Item.Name
		}
		needQty[l.Item.ID] += l.Qty
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	type itemSnap struct {
		name      string
		cost      int
		origStock int
		curStock  int
		origAct   bool
		curAct    bool
	}
	snaps := make(map[int64]*itemSnap, len(ids))
	rows, err := tx.Query(ctx, `select id, name, cost_cents, stock, active, archived_at is not null
		from retail_items where id = any($1::bigint[]) order by id for update`, ids)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var id int64
		var s itemSnap
		var archived bool
		if err := rows.Scan(&id, &s.name, &s.cost, &s.origStock, &s.origAct, &archived); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if archived {
			rows.Close()
			return 0, nil, platform.UserErr("「" + s.name + "」已停用删除，不能售出")
		}
		if s.cost < 0 {
			rows.Close()
			return 0, nil, platform.UserErr("「" + s.name + "」成本不能为负")
		}
		if s.origStock < needQty[id] {
			rows.Close()
			return 0, nil, platform.UserErr("「" + reqName[id] + "」库存不够了")
		}
		s.curStock = s.origStock
		s.curAct = s.origAct
		snaps[id] = &s
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	for _, id := range ids {
		if _, ok := snaps[id]; !ok {
			return 0, nil, platform.UserErr("「" + reqName[id] + "」不存在")
		}
	}

	type line struct {
		id               int64
		name             string
		price, cost, qty int
	}
	sold := make([]line, 0, len(lines))
	var total, cost int
	for _, l := range lines {
		s := snaps[l.Item.ID]
		if _, err := tx.Exec(ctx, `update retail_items
			set stock = stock - $2,
			    active = case when stock - $2 > 0 then active else false end
			where id=$1`, l.Item.ID, l.Qty); err != nil {
			return 0, nil, err
		}
		s.curStock -= l.Qty
		if s.curStock == 0 {
			s.curAct = false
		}
		sold = append(sold, line{id: l.Item.ID, name: s.name, price: l.Item.PriceCents, cost: s.cost, qty: l.Qty})
		total += l.Item.PriceCents * l.Qty
		cost += s.cost * l.Qty
	}

	var low []LowStockItem
	for _, id := range ids {
		s := snaps[id]
		if s.origAct && !s.curAct {
			if err := platform.LogOperation(ctx, tx, platform.OpRetailOff, platform.RetailSubject(s.name), "下架 · 售罄"); err != nil {
				return 0, nil, err
			}
		}
		if (s.origStock > LowStockThreshold && s.curStock <= LowStockThreshold) || (s.origStock > 0 && s.curStock == 0) {
			low = append(low, LowStockItem{ItemID: id, Name: s.name, Stock: s.curStock, Active: s.curAct})
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

// RetailStatsSummary 今日与本月零售营收/成本（按营业日 07:00–次日 07:00 划分，与经营概览同源）。
func RetailStatsSummary(ctx context.Context, pool *pgxpool.Pool) (RetailStats, error) {
	todayStart := platform.BusinessDayStart(time.Now())
	tomorrowStart := todayStart.AddDate(0, 0, 1)
	monthStart := time.Date(todayStart.Year(), todayStart.Month(), 1, platform.BusinessDayStartHour, 0, 0, 0, platform.ReportZone)

	var st RetailStats
	err := pool.QueryRow(ctx, `
		select coalesce(sum(total_cents) filter (where created_at >= $1 and created_at < $2), 0)::int,
		       coalesce(sum(cost_cents)  filter (where created_at >= $1 and created_at < $2), 0)::int,
		       coalesce(sum(total_cents) filter (where created_at >= $3 and created_at < $2), 0)::int,
		       coalesce(sum(cost_cents)  filter (where created_at >= $3 and created_at < $2), 0)::int
		from retail_sales
		where created_at >= $3 and created_at < $2`, todayStart, tomorrowStart, monthStart).Scan(&st.TodayRev, &st.TodayCost, &st.MonthRev, &st.MonthCost)
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
		where archived_at is null
		  and ($1 = '' or name ilike '%'||$1||'%' or category ilike '%'||$1||'%' or spec ilike '%'||$1||'%')
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

	var oldActive, archived bool
	var oldStock int
	var name string
	err = tx.QueryRow(ctx, `select active, stock, name, archived_at is not null from retail_items where id=$1 for update`, id).
		Scan(&oldActive, &oldStock, &name, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.UserErr("商品不存在")
	}
	if err != nil {
		return nil, err
	}
	if archived {
		return nil, platform.UserErr("商品已停用删除，无法调整库存")
	}
	if oldStock+delta < 0 {
		return nil, platform.UserErr("库存不够扣，先核对数量")
	}
	var stock int
	var active bool
	err = tx.QueryRow(ctx, `update retail_items
		set stock = stock + $2,
		    active = case when stock + $2 > 0 then active else false end
		where id=$1 and stock + $2 >= 0
		returning stock, active`, id, delta).Scan(&stock, &active)
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
	if delta < 0 && ((oldStock > LowStockThreshold && stock <= LowStockThreshold) || (oldStock > 0 && stock == 0)) {
		return &LowStockItem{ItemID: id, Name: name, Stock: stock, Active: active}, nil
	}
	return nil, nil
}
