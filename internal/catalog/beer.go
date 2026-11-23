package catalog

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
)

// Beer 是后台编辑、酒单展示共用的酒款记录。可空字段用指针，让 JSON 能省略。
type Beer struct {
	ID         int64
	No         string
	Brewery    string
	Name       string
	Style      string
	Category   string
	ABV        *float64
	IBU        *int
	Size       string
	PriceCents int // 酒单展示用的主价，由三种杯型折算
	Price150   int // 0 表示不提供这个杯型
	Price330   int
	Price500   int
	UT         *float64
	UTCount    *int
	Tags       []string
	SalesTags  []string
	PosterURL  string
	OnScreen   bool
	SortOrder  int
}

func (b Beer) Price150Input() string { return platform.MoneyInput(b.Price150) }
func (b Beer) Price330Input() string { return platform.MoneyInput(b.Price330) }
func (b Beer) Price500Input() string { return platform.MoneyInput(b.Price500) }
func (b Beer) ABVText() string       { return platform.StatText(b.ABV, "%", 1) }
func (b Beer) IBUText() string       { return platform.StatText(b.IBU, "", 0) }
func (b Beer) UTText() string        { return platform.StatText(b.UT, "", 2) }
func (b Beer) ABVInput() string      { return platform.NumText(b.ABV, 1) }
func (b Beer) IBUInput() string      { return platform.NumText(b.IBU, 0) }
func (b Beer) UTInput() string       { return platform.NumText(b.UT, 2) }
func (b Beer) UTCountInput() string  { return platform.NumText(b.UTCount, 0) }
func (b Beer) TagsText() string      { return strings.Join(b.Tags, "、") }
func (b Beer) SalesText() string     { return strings.Join(b.SalesTags, "、") }
func (b Beer) CategoryName() string  { return platform.CategoryLabel(b.Category) }

// CupSizes 是店里开放的杯型；某款酒不提供某个杯型就留空价。
var CupSizes = []int{150, 330, 500}

// Cup 是「杯型 + 这一杯的价」。
type Cup struct {
	ML    int
	Cents int
}

func (c Cup) Price() string  { return platform.PriceText(c.Cents) }
func (c Cup) MLText() string { return strconv.Itoa(c.ML) + "ml" }

func (b Beer) Cups() []Cup {
	out := make([]Cup, 0, len(CupSizes))
	for _, ml := range CupSizes {
		if c := b.CupAt(ml); c.Cents > 0 {
			out = append(out, c)
		}
	}
	return out
}

func (b Beer) CupAt(ml int) Cup {
	switch ml {
	case 150:
		return Cup{ML: 150, Cents: b.Price150}
	case 330:
		return Cup{ML: 330, Cents: b.Price330}
	case 500:
		return Cup{ML: 500, Cents: b.Price500}
	}
	return Cup{ML: ml}
}

// SyncPrice 把三种杯型折算成酒单要用的「一个容量 + 一个价」：有 330 就用 330，否则用最便宜的一杯。
func (b *Beer) SyncPrice() {
	cups := b.Cups()
	if len(cups) == 0 {
		b.Size, b.PriceCents = "", 0
		return
	}
	mls := make([]string, len(cups))
	for i, c := range cups {
		mls[i] = strconv.Itoa(c.ML)
	}
	b.Size = strings.Join(mls, "/") + "ml"
	b.PriceCents = cups[0].Cents
	for _, c := range cups {
		if c.ML == 330 {
			b.PriceCents = c.Cents
			break
		}
		b.PriceCents = min(b.PriceCents, c.Cents)
	}
}

func cupPtr(cents int) *int {
	if cents <= 0 {
		return nil
	}
	return &cents
}

const beerCols = `id, no, brewery, name, style, category, abv, ibu, size, price_cents,
	coalesce(price_150, 0), coalesce(price_330, 0), coalesce(price_500, 0),
	ut_rating, ut_count, tags, sales_tags, poster_url, on_screen, sort_order`

func scanBeer(row pgx.Row) (Beer, error) {
	var b Beer
	err := row.Scan(&b.ID, &b.No, &b.Brewery, &b.Name, &b.Style, &b.Category, &b.ABV, &b.IBU,
		&b.Size, &b.PriceCents, &b.Price150, &b.Price330, &b.Price500,
		&b.UT, &b.UTCount, &b.Tags, &b.SalesTags, &b.PosterURL, &b.OnScreen, &b.SortOrder)
	return b, err
}

func ListBeers(ctx context.Context, pool *pgxpool.Pool, onScreenOnly bool) ([]Beer, error) {
	q := `select ` + beerCols + ` from beers`
	if onScreenOnly {
		q += ` where on_screen`
	}
	rows, err := pool.Query(ctx, q+` order by sort_order, no, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Beer
	for rows.Next() {
		b, err := scanBeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func GetBeer(ctx context.Context, pool *pgxpool.Pool, id int64) (Beer, error) {
	return scanBeer(pool.QueryRow(ctx, `select `+beerCols+` from beers where id=$1`, id))
}

func FindBeerForUpdate(ctx context.Context, tx pgx.Tx, brewery, name string) (Beer, error) {
	return scanBeer(tx.QueryRow(ctx, `select `+beerCols+` from beers
		where lower(btrim(brewery)) = lower(btrim($1))
		  and lower(btrim(name))    = lower(btrim($2))
		order by id asc
		limit 1
		for update`, brewery, name))
}

func SaveBeer(ctx context.Context, q platform.Querier, b Beer) (int64, error) {
	b.Brewery = strings.TrimSpace(b.Brewery)
	b.Name = strings.TrimSpace(b.Name)
	b.Style = strings.TrimSpace(b.Style)
	b.PosterURL = strings.TrimSpace(b.PosterURL)
	cat, err := platform.ValidateBeerFields(b.Name, b.Category, b.PosterURL, b.ABV, b.UT, b.IBU, b.UTCount, b.Price150, b.Price330, b.Price500, b.SortOrder)
	if err != nil {
		return 0, err
	}
	b.Category = cat
	if b.Category == "" {
		b.Category = platform.ToneOf(b.Style)
	}
	if b.Tags == nil {
		b.Tags = []string{}
	}
	if b.SalesTags == nil {
		b.SalesTags = []string{}
	}
	b.SyncPrice()
	if b.ID == 0 {
		err := q.QueryRow(ctx, `insert into beers
			(no, brewery, name, style, category, abv, ibu, size, price_cents, price_150, price_330, price_500,
			 ut_rating, ut_count, tags, sales_tags, poster_url, on_screen, sort_order)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) returning id`,
			b.No, b.Brewery, b.Name, b.Style, b.Category, b.ABV, b.IBU, b.Size, b.PriceCents,
			cupPtr(b.Price150), cupPtr(b.Price330), cupPtr(b.Price500),
			b.UT, b.UTCount, b.Tags, b.SalesTags, b.PosterURL, b.OnScreen, b.SortOrder).Scan(&b.ID)
		return b.ID, err
	}
	tag, err := q.Exec(ctx, `update beers set
		no=$1, brewery=$2, name=$3, style=$4, category=$5, abv=$6, ibu=$7, size=$8, price_cents=$9,
		price_150=$10, price_330=$11, price_500=$12,
		ut_rating=$13, ut_count=$14, tags=$15, sales_tags=$16, poster_url=$17, on_screen=$18,
		sort_order=$19, updated_at=now() where id=$20`,
		b.No, b.Brewery, b.Name, b.Style, b.Category, b.ABV, b.IBU, b.Size, b.PriceCents,
		cupPtr(b.Price150), cupPtr(b.Price330), cupPtr(b.Price500),
		b.UT, b.UTCount, b.Tags, b.SalesTags, b.PosterURL, b.OnScreen, b.SortOrder, b.ID)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, platform.UserErr("酒款不存在")
	}
	return b.ID, nil
}

// DeleteBeer 禁止删除已有售出流水的酒款，防止级联擦除历史营业额与桶装记录。
func DeleteBeer(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var lockedID int64
	if err := tx.QueryRow(ctx, `select id from beers where id=$1 for update`, id).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return platform.UserErr("酒款不存在")
		}
		return err
	}

	var hasPours bool
	if err := tx.QueryRow(ctx, `
		select exists(
			select 1 from keg_pours p
			join kegs k on k.id = p.keg_id
			where k.beer_id = $1
		)`, id).Scan(&hasPours); err != nil {
		return err
	}
	if hasPours {
		return platform.UserErr("有售出记录的酒款不能删除，下架保留即可")
	}

	if _, err := tx.Exec(ctx, `delete from beers where id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ToggleBeer(ctx context.Context, pool *pgxpool.Pool, id int64) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var on bool
	var brewery, name string
	err = tx.QueryRow(ctx, `update beers set on_screen = not on_screen, updated_at=now() where id=$1
		returning on_screen, coalesce(brewery,''), name`, id).Scan(&on, &brewery, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, platform.UserErr("酒款不存在")
	}
	if err != nil {
		return false, err
	}
	kind, detail := platform.OpScreenOff, "下屏"
	if on {
		kind, detail = platform.OpScreenOn, "上屏"
	}
	if err := platform.LogOperation(ctx, tx, kind, platform.BeerSubject(brewery, name), detail); err != nil {
		return false, err
	}
	return on, tx.Commit(ctx)
}
