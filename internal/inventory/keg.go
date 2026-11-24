package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/platform"
)

// Keg 是一桶酒的生命周期：上架 → 开桶 → 逐笔售出 → 结桶，盈亏由售出金额减成本得出。
type Keg struct {
	ID         int64
	BeerID     int64
	BeerNo     string
	Brewery    string
	BeerName   string
	Style      string
	KegNo      string
	CapacityML int
	CostCents  int
	PlacedAt   *time.Time
	TappedAt   *time.Time
	FinishedAt *time.Time
	Note       string

	SoldCups     int
	SoldML       int
	RevenueCents int
	PourCount    int
}

const kegSelect = `
select k.id, k.beer_id, coalesce(b.no,''), b.brewery, b.name, b.style, k.keg_no,
       k.capacity_ml, k.cost_cents, k.placed_at, k.tapped_at, k.finished_at, k.note,
       coalesce(sum(p.cups),0)::int, coalesce(sum(p.ml),0)::int, coalesce(sum(p.amount_cents),0)::int,
       count(p.id)::int
  from kegs k
  join beers b on b.id = k.beer_id
  left join keg_pours p on p.keg_id = k.id`

const kegGroup = ` group by k.id, b.id`

func scanKeg(row interface {
	Scan(dest ...any) error
}) (Keg, error) {
	var k Keg
	err := row.Scan(&k.ID, &k.BeerID, &k.BeerNo, &k.Brewery, &k.BeerName, &k.Style, &k.KegNo,
		&k.CapacityML, &k.CostCents, &k.PlacedAt, &k.TappedAt, &k.FinishedAt, &k.Note,
		&k.SoldCups, &k.SoldML, &k.RevenueCents, &k.PourCount)
	return k, err
}

func ListKegs(ctx context.Context, pool *pgxpool.Pool, onlyActive bool) ([]Keg, error) {
	q := kegSelect
	if onlyActive {
		// 在售＝已开桶且没停售；进了货还没开的那几桶不算。
		q += ` where k.tapped_at is not null and k.finished_at is null`
	}
	q += kegGroup + ` order by (k.finished_at is null and k.tapped_at is not null) desc, k.placed_at desc nulls last, k.id desc`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Keg
	for rows.Next() {
		k, err := scanKeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func GetKeg(ctx context.Context, pool *pgxpool.Pool, id int64) (Keg, error) {
	return scanKeg(pool.QueryRow(ctx, kegSelect+` where k.id=$1`+kegGroup, id))
}

// SaveKeg 新建或更新一桶；beer_id 必须指向已存在的酒款。
// 先按 id 升序锁 beers 行（和 TapBeer/UntapBeerResult 同序），再锁桶行，
// 所以后台手工把一桶填成「在售」也要过单在售桶的检查。
func SaveKeg(ctx context.Context, pool *pgxpool.Pool, k Keg) (int64, error) {
	if k.BeerID == 0 {
		return 0, platform.UserErr("先选一款酒")
	}
	if k.CapacityML <= 0 {
		return 0, platform.UserErr("桶容量要大于 0")
	}
	if k.CostCents < 0 {
		return 0, platform.UserErr("成本不能为负")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	beerIDs := []int64{k.BeerID}
	var oldBeerID int64
	var oldTapped, oldFinished bool
	if k.ID != 0 {
		err = tx.QueryRow(ctx, `select beer_id, tapped_at is not null, finished_at is not null
			from kegs where id=$1`, k.ID).Scan(&oldBeerID, &oldTapped, &oldFinished)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, platform.UserErr("酒桶不存在")
		}
		if err != nil {
			return 0, err
		}
		if oldBeerID != k.BeerID {
			beerIDs = append(beerIDs, oldBeerID)
		}
	}
	var locked int
	if err := tx.QueryRow(ctx, `select count(*)::int from (
		select id from beers where id = any($1) order by id for update) b`, beerIDs).Scan(&locked); err != nil {
		return 0, err
	}
	if locked != len(beerIDs) {
		return 0, platform.UserErr("酒款不存在")
	}

	var brewery, name string
	if err := tx.QueryRow(ctx, `select coalesce(brewery,''), name from beers where id=$1`, k.BeerID).
		Scan(&brewery, &name); err != nil {
		return 0, err
	}
	if k.ID != 0 {
		var beerID int64
		if err := tx.QueryRow(ctx, `select beer_id from kegs where id=$1 for update`, k.ID).Scan(&beerID); err != nil {
			return 0, err
		}
		if beerID != oldBeerID {
			return 0, platform.UserErr("这桶刚被改过，重试一下")
		}
	}
	// 手工填了开桶时间就是在开桶，得和在售的那桶互斥。
	if k.TappedAt != nil && k.FinishedAt == nil {
		var active bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from kegs
			where beer_id=$1 and tapped_at is not null and finished_at is null and id <> $2)`,
			k.BeerID, k.ID).Scan(&active); err != nil {
			return 0, err
		}
		if active {
			return 0, platform.UserErr("这款酒已经有在售桶了，先在酒单里下架那桶")
		}
	}
	if k.ID == 0 {
		var id int64
		if err := tx.QueryRow(ctx, `insert into kegs (beer_id, keg_no, capacity_ml, cost_cents, placed_at, tapped_at, finished_at, note)
			values ($1,$2,$3,$4,$5,$6,$7,$8) returning id`,
			k.BeerID, k.KegNo, k.CapacityML, k.CostCents, k.PlacedAt, k.TappedAt, k.FinishedAt, k.Note).Scan(&id); err != nil {
			return 0, err
		}
		detail := fmt.Sprintf("新建酒桶 #%d · %s · %dml · 进价 %s", id, k.Status(), k.CapacityML, platform.PriceText(k.CostCents))
		if err := platform.LogOperation(ctx, tx, platform.OpStock, platform.BeerSubject(brewery, name), detail); err != nil {
			return 0, err
		}
		return id, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `update kegs set beer_id=$2, keg_no=$3, capacity_ml=$4, cost_cents=$5,
		placed_at=$6, tapped_at=$7, finished_at=$8, note=$9 where id=$1`,
		k.ID, k.BeerID, k.KegNo, k.CapacityML, k.CostCents, k.PlacedAt, k.TappedAt, k.FinishedAt, k.Note); err != nil {
		return 0, err
	}
	// 只有真实的状态翻转才写流水；改容量、成本、备注这些 metadata 不记。
	switch {
	case k.FinishedAt != nil && !oldFinished:
		if err := platform.LogOperation(ctx, tx, platform.OpUntap, platform.BeerSubject(brewery, name),
			fmt.Sprintf("编辑结桶 · 桶 #%d", k.ID)); err != nil {
			return 0, err
		}
	case k.FinishedAt == nil && k.TappedAt != nil && (!oldTapped || oldFinished):
		if err := platform.LogOperation(ctx, tx, platform.OpTap, platform.BeerSubject(brewery, name),
			fmt.Sprintf("编辑开桶 · 桶 #%d", k.ID)); err != nil {
			return 0, err
		}
	}
	return k.ID, tx.Commit(ctx)
}

func DeleteKeg(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `delete from kegs where id=$1`, id)
	return err
}

type Pour struct {
	ID          int64
	KegID       int64
	Cups        int
	ML          int
	AmountCents int
	PouredAt    time.Time
}

func (p Pour) PouredText() string { return p.PouredAt.Format("01-02 15:04") }
func (p Pour) Amount() string     { return platform.PriceText(p.AmountCents) }

func ListPours(ctx context.Context, pool *pgxpool.Pool, kegID int64) ([]Pour, error) {
	rows, err := pool.Query(ctx, `select id, keg_id, cups, ml, amount_cents, poured_at
		from keg_pours where keg_id=$1 order by poured_at desc, id desc`, kegID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pour
	for rows.Next() {
		var p Pour
		if err := rows.Scan(&p.ID, &p.KegID, &p.Cups, &p.ML, &p.AmountCents, &p.PouredAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPourTx 在调用方事务里记一笔售出；ml 是本次售出的总毫升。
// 锁住桶行，与结桶（UntapBeerResult 写 finished_at）互斥。
func AddPourTx(ctx context.Context, tx pgx.Tx, kegID int64, cups, ml, amountCents int) error {
	if cups <= 0 {
		return platform.UserErr("杯数要大于 0")
	}
	if ml < 0 || amountCents < 0 {
		return platform.UserErr("毫升和金额不能为负")
	}
	if ml == 0 && amountCents == 0 {
		return platform.UserErr("毫升和金额至少填一个")
	}
	var tapped, finished bool
	err := tx.QueryRow(ctx, `select tapped_at is not null, finished_at is not null
		from kegs where id=$1 for update`, kegID).Scan(&tapped, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("这桶不存在")
	}
	if err != nil {
		return err
	}
	if finished {
		return platform.UserErr("这桶已结桶，不能再记售出")
	}
	if !tapped {
		return platform.UserErr("这桶还没开，先上架开桶")
	}
	_, err = tx.Exec(ctx, `insert into keg_pours (keg_id, cups, ml, amount_cents) values ($1,$2,$3,$4)`,
		kegID, cups, ml, amountCents)
	return err
}

func AddPour(ctx context.Context, pool *pgxpool.Pool, kegID int64, cups, ml, amountCents int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := AddPourTx(ctx, tx, kegID, cups, ml, amountCents); err != nil {
		return err
	}
	var brewery, name string
	if err := tx.QueryRow(ctx, `select coalesce(b.brewery,''), b.name
		from kegs k join beers b on b.id = k.beer_id where k.id=$1`, kegID).Scan(&brewery, &name); err != nil {
		return err
	}
	detail := fmt.Sprintf("%d 杯 · %dml · %s", cups, ml, platform.PriceText(amountCents))
	if err := platform.LogOperation(ctx, tx, platform.OpManualSale, platform.BeerSubject(brewery, name), detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeletePour 删一笔手工售出；先 DELETE ... RETURNING 拿到快照，删不存在的记录直接报错，
// 不会凭空写一条撤销流水。
func DeletePour(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var p Pour
	err = tx.QueryRow(ctx, `delete from keg_pours where id=$1
		returning keg_id, cups, ml, amount_cents`, id).Scan(&p.KegID, &p.Cups, &p.ML, &p.AmountCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return platform.UserErr("这笔记售出不存在")
	}
	if err != nil {
		return err
	}
	var brewery, name string
	if err := tx.QueryRow(ctx, `select coalesce(b.brewery,''), b.name
		from kegs k join beers b on b.id = k.beer_id where k.id=$1`, p.KegID).Scan(&brewery, &name); err != nil {
		return err
	}
	detail := fmt.Sprintf("删除售出 %d 杯 · %dml · %s", p.Cups, p.ML, platform.PriceText(p.AmountCents))
	if err := platform.LogOperation(ctx, tx, platform.OpSaleVoid, platform.BeerSubject(brewery, name), detail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// KegWarnSoldPct 表示在售酒桶走量告警阈值（超过该百分比触发告警，与 SoldPct 整数口径对齐）。
const KegWarnSoldPct = 50

// 派生值只在这里算一次，模板和接口读同一份。

func (k Keg) Revenue() string     { return platform.PriceText(k.RevenueCents) }
func (k Keg) Cost() string        { return platform.PriceText(k.CostCents) }
func (k Keg) ProfitCents() int    { return k.RevenueCents - k.CostCents }
func (k Keg) Profit() string      { return platform.MoneyText(k.ProfitCents()) }
func (k Keg) Profitable() bool    { return k.ProfitCents() >= 0 }
func (k Keg) RemainingML() int    { return max(k.CapacityML-k.SoldML, 0) }
func (k Keg) SoldPct() int        { return platform.ClampPct(k.SoldML * 100 / max(k.CapacityML, 1)) }
func (k Keg) IsSoldWarning() bool { return k.CanPour() && k.SoldPct() > KegWarnSoldPct }
func (k Keg) AvgMLPerCup() int    { return k.SoldML / max(k.SoldCups, 1) }
func (k Keg) PlacedInput() string { return platform.TimeInput(k.PlacedAt) }
func (k Keg) TappedInput() string { return platform.TimeInput(k.TappedAt) }
func (k Keg) FinishedInput() string {
	return platform.TimeInput(k.FinishedAt)
}
func (k Keg) PlacedText() string   { return platform.TimeText(k.PlacedAt) }
func (k Keg) TappedText() string   { return platform.TimeText(k.TappedAt) }
func (k Keg) FinishedText() string { return platform.TimeText(k.FinishedAt) }
func (k Keg) Active() bool         { return k.FinishedAt == nil }
func (k Keg) CanPour() bool        { return k.TappedAt != nil && k.FinishedAt == nil }
func (k Keg) AvgCupPrice() string {
	if k.SoldCups == 0 {
		return "—"
	}
	return platform.PriceText(k.RevenueCents / k.SoldCups)
}

func (k Keg) Status() string {
	switch {
	case k.FinishedAt != nil:
		return "停售"
	case k.TappedAt != nil:
		return "在售"
	case k.PlacedAt != nil:
		return "已上架"
	default:
		return "待上架"
	}
}

func (k Keg) Badge() string {
	switch k.Status() {
	case "在售":
		return "bg-success"
	case "停售":
		return "bg-secondary"
	case "已上架":
		return "bg-warning text-dark"
	default:
		return "bg-primary"
	}
}

func (k Keg) Title() string {
	if k.KegNo == "" {
		return k.Brewery + " · " + k.BeerName
	}
	return k.Brewery + " · " + k.BeerName + "（" + k.KegNo + "）"
}

// PurchaseKegs 进货：把 count 桶未开的酒记进库存（未上架、未开桶）。
// db 可以是连接池，也可以是调用方事务，便于和保存酒款一起提交。
func PurchaseKegs(ctx context.Context, db platform.Querier, beerID int64, count, capacityML, costCents int) error {
	if count < 1 {
		return platform.UserErr("进货数量至少 1 桶")
	}
	if capacityML <= 0 {
		capacityML = catalog.DefaultCapacityML
	}
	if costCents < 0 {
		costCents = 0
	}
	for range count {
		if _, err := db.Exec(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, placed_at) values ($1,$2,$3,now())`,
			beerID, capacityML, costCents); err != nil {
			return err
		}
	}
	return nil
}

// StockCounts 每款酒还有几桶没开的库存。
func StockCounts(ctx context.Context, pool *pgxpool.Pool) (map[int64]int, error) {
	rows, err := pool.Query(ctx, `select beer_id, count(*)::int from kegs where tapped_at is null and finished_at is null group by beer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

type TapResult struct {
	BeerID    int64
	KegID     int64
	Brewery   string
	Name      string
	No        string
	OpenedNew bool
}

// TapBeerResult 上架：写号位并上屏；已有在售桶就不再开桶，否则开最早一桶库存，没库存才新建。
func TapBeerResult(ctx context.Context, pool *pgxpool.Pool, beerID int64, no string, capacityML, costCents int) (TapResult, error) {
	no = strings.TrimSpace(no)
	if no == "" {
		return TapResult{}, platform.UserErr("要填上架的号位")
	}
	if capacityML <= 0 {
		capacityML = catalog.DefaultCapacityML
	}
	if costCents < 0 {
		costCents = 0
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return TapResult{}, err
	}
	defer tx.Rollback(ctx)

	// 更新酒款行会锁住它，同一款酒的上架串行执行，不会并发开出两桶。
	var brewery, name string
	err = tx.QueryRow(ctx, `update beers set no=$2, on_screen=true, updated_at=now() where id=$1
		returning coalesce(brewery,''), name`, beerID, no).Scan(&brewery, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return TapResult{}, platform.UserErr("酒款不存在")
	}
	if err != nil {
		return TapResult{}, err
	}
	var activeKegID int64
	err = tx.QueryRow(ctx, `select id from kegs
		where beer_id=$1 and tapped_at is not null and finished_at is null
		limit 1`, beerID).Scan(&activeKegID)
	if err == nil {
		// 只是改了号位/上屏，没有真实开桶，不记开桶流水。
		if err := tx.Commit(ctx); err != nil {
			return TapResult{}, err
		}
		return TapResult{BeerID: beerID, KegID: activeKegID, Brewery: brewery, Name: name, No: no, OpenedNew: false}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TapResult{}, err
	}
	// 有库存就开最早的那一桶，没有才现开一桶。
	var kegID int64
	var capacity, cost int
	err = tx.QueryRow(ctx, `update kegs set tapped_at=now()
		where id = (select id from kegs where beer_id=$1 and tapped_at is null and finished_at is null
			order by id limit 1)
		returning id, capacity_ml, cost_cents`, beerID).Scan(&kegID, &capacity, &cost)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `insert into kegs (beer_id, capacity_ml, cost_cents, placed_at, tapped_at)
			values ($1,$2,$3,now(),now()) returning id`, beerID, capacityML, costCents).Scan(&kegID)
		capacity, cost = capacityML, costCents
	}
	if err != nil {
		return TapResult{}, err
	}
	detail := fmt.Sprintf("号位 %s · 桶 #%d · %dml · 进价 %s", no, kegID, capacity, platform.PriceText(cost))
	if err := platform.LogOperation(ctx, tx, platform.OpTap, platform.BeerSubject(brewery, name), detail); err != nil {
		return TapResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TapResult{}, err
	}
	return TapResult{BeerID: beerID, KegID: kegID, Brewery: brewery, Name: name, No: no, OpenedNew: true}, nil
}

func TapBeer(ctx context.Context, pool *pgxpool.Pool, beerID int64, no string, capacityML, costCents int) error {
	_, err := TapBeerResult(ctx, pool, beerID, no, capacityML, costCents)
	return err
}

// LatestFinishedKegs 查询每款酒最近一次已下架（结桶）的记录，供酒单页在无在售桶时回看上次结桶盈亏。
func LatestFinishedKegs(ctx context.Context, pool *pgxpool.Pool) (map[int64]*Keg, error) {
	rows, err := pool.Query(ctx, `with latest as (
		select distinct on (beer_id) id
		from kegs
		where finished_at is not null
		order by beer_id, finished_at desc, id desc
	) `+kegSelect+` where k.id in (select id from latest)`+kegGroup)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*Keg{}
	for rows.Next() {
		k, err := scanKeg(rows)
		if err != nil {
			return nil, err
		}
		kc := k
		out[kc.BeerID] = &kc
	}
	return out, rows.Err()
}

// UntapBeer 下架：从酒单撤下，并把在售的那一桶记成停售（还没开的库存酒不动）。
func UntapBeer(ctx context.Context, pool *pgxpool.Pool, beerID int64) error {
	_, err := UntapBeerResult(ctx, pool, beerID)
	return err
}

// UntapBeerResult 下架并返回实际被结桶的桶 ID（没有在售桶则 0）。
func UntapBeerResult(ctx context.Context, pool *pgxpool.Pool, beerID int64) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var brewery, name string
	err = tx.QueryRow(ctx, `update beers set on_screen=false, updated_at=now() where id=$1
		returning coalesce(brewery,''), name`, beerID).Scan(&brewery, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	var kegID int64
	err = tx.QueryRow(ctx, `update kegs set finished_at=now()
		where beer_id=$1 and tapped_at is not null and finished_at is null returning id`, beerID).Scan(&kegID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	if err := platform.LogOperation(ctx, tx, platform.OpUntap, platform.BeerSubject(brewery, name),
		fmt.Sprintf("桶 #%d 结桶停售", kegID)); err != nil {
		return 0, err
	}
	return kegID, tx.Commit(ctx)
}

// PurchaseBeerTx 在同一个事务里复用或新建酒款、记库存桶、写入流水并沉淀到历史酒单：
// 任何一步失败都整体回滚，不会留下没有库存的酒款，重试也不会多建一款。
func PurchaseBeerTx(ctx context.Context, pool *pgxpool.Pool, b catalog.Beer, count, capacityML, costCents int) (int64, error) {
	b.Brewery = strings.TrimSpace(b.Brewery)
	b.Name = strings.TrimSpace(b.Name)
	b.Style = strings.TrimSpace(b.Style)
	b.PosterURL = strings.TrimSpace(b.PosterURL)
	if _, err := platform.ValidateBeerFields(b.Name, b.Category, b.PosterURL, b.ABV, b.UT, b.IBU, b.UTCount, b.Price150, b.Price330, b.Price500, b.SortOrder); err != nil {
		return 0, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // 已提交时回滚是空操作

	// 按「酒款行 → 桶行」顺序先锁定同名酒款：补货已有酒款时复用同一行并保留号位/上屏设置。
	target := b
	existing, err := catalog.FindBeerForUpdate(ctx, tx, b.Brewery, b.Name)
	switch {
	case err == nil:
		merged := existing
		if b.Brewery != "" {
			merged.Brewery = b.Brewery
		}
		merged.Name = b.Name
		if b.Style != "" {
			merged.Style = b.Style
		}
		if b.Category != "" {
			merged.Category = b.Category
		}
		if b.PosterURL != "" {
			merged.PosterURL = b.PosterURL
		}
		if b.ABV != nil {
			merged.ABV = b.ABV
		}
		if b.IBU != nil {
			merged.IBU = b.IBU
		}
		if b.UT != nil {
			merged.UT = b.UT
		}
		if b.UTCount != nil {
			merged.UTCount = b.UTCount
		}
		if len(b.Tags) > 0 {
			merged.Tags = b.Tags
		}
		if b.Price150 > 0 || b.Price330 > 0 || b.Price500 > 0 {
			merged.Price150 = b.Price150
			merged.Price330 = b.Price330
			merged.Price500 = b.Price500
		}
		target = merged
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, err
	}

	id, err := catalog.SaveBeer(ctx, tx, target)
	if err != nil {
		return 0, err
	}
	if err := PurchaseKegs(ctx, tx, id, count, capacityML, costCents); err != nil {
		return 0, err
	}
	detail := fmt.Sprintf("%d 桶 × %dml，进价 %s/桶", count, capacityML, platform.PriceText(costCents))
	if err := platform.LogOperation(ctx, tx, platform.OpPurchase, platform.BeerSubject(target.Brewery, target.Name), detail); err != nil {
		return 0, err
	}
	if err := catalog.UpsertCatalogFromBeer(ctx, tx, target, capacityML, costCents, true); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// LatestBeerKegSpec 查询酒款当前或最近关联的桶容与进价（缺失时回退历史酒单或默认 20000ml），供编辑页实时测算用。
func LatestBeerKegSpec(ctx context.Context, pool *pgxpool.Pool, b catalog.Beer) (Keg, string) {
	var keg Keg
	var costCents int
	err := pool.QueryRow(ctx, `
		select id, capacity_ml, cost_cents
		from kegs
		where beer_id = $1
		order by (tapped_at is not null and finished_at is null) desc, id desc
		limit 1`, b.ID).Scan(&keg.ID, &keg.CapacityML, &costCents)
	if err == pgx.ErrNoRows && strings.TrimSpace(b.Name) != "" {
		err = pool.QueryRow(ctx, `
			select capacity_ml, cost_cents
			from beer_catalog
			where lower(btrim(brewery)) = lower(btrim($1)) and lower(btrim(name)) = lower(btrim($2))
			limit 1`, b.Brewery, b.Name).Scan(&keg.CapacityML, &costCents)
	}
	if err != nil && err != pgx.ErrNoRows {
		keg = Keg{}
		costCents = 0
	}
	if keg.CapacityML <= 0 {
		keg.CapacityML = catalog.DefaultCapacityML
	}
	return keg, platform.MoneyInput(costCents)
}
