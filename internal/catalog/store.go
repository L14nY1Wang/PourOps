package catalog

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
)

const (
	DefaultCapacityML     = 20000
	ImportMaxBytes        = 5 << 20  // 5 MiB CSV / catalog.json 单文件上限
	ImportArchiveMaxBytes = 50 << 20 // 50 MiB 压缩包上传上限
	ImportMaxRows         = 2000     // 单次导入最多 2000 款
)

// CatalogBeer 是门店自己的历史酒单主档，与在售/库存的 beers 表解耦。
type CatalogBeer struct {
	ID         int64
	Brewery    string
	Name       string
	Style      string
	Category   string
	ABV        *float64
	IBU        *int
	UT         *float64
	UTCount    *int
	Tags       []string
	PosterURL  string
	CapacityML int
	CostCents  int
	Price150   int
	Price330   int
	Price500   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (c CatalogBeer) ABVText() string       { return platform.StatText(c.ABV, "%", 1) }
func (c CatalogBeer) IBUText() string       { return platform.StatText(c.IBU, "", 0) }
func (c CatalogBeer) UTText() string        { return platform.StatText(c.UT, "", 2) }
func (c CatalogBeer) ABVInput() string      { return platform.NumText(c.ABV, 1) }
func (c CatalogBeer) IBUInput() string      { return platform.NumText(c.IBU, 0) }
func (c CatalogBeer) UTInput() string       { return platform.NumText(c.UT, 2) }
func (c CatalogBeer) UTCountInput() string  { return platform.NumText(c.UTCount, 0) }
func (c CatalogBeer) TagsText() string      { return strings.Join(c.Tags, "、") }
func (c CatalogBeer) CategoryName() string  { return platform.CategoryLabel(c.Category) }
func (c CatalogBeer) CostInput() string     { return platform.MoneyInput(c.CostCents) }
func (c CatalogBeer) Price150Input() string { return platform.MoneyInput(c.Price150) }
func (c CatalogBeer) Price330Input() string { return platform.MoneyInput(c.Price330) }
func (c CatalogBeer) Price500Input() string { return platform.MoneyInput(c.Price500) }

func (c CatalogBeer) CostText() string {
	if c.CostCents <= 0 {
		return "—"
	}
	return platform.PriceText(c.CostCents)
}

func (c CatalogBeer) UpdatedText() string {
	if c.UpdatedAt.IsZero() {
		return "—"
	}
	return c.UpdatedAt.In(platform.ReportZone).Format("2006-01-02 15:04")
}

func (c CatalogBeer) Cups() []Cup {
	return Beer{Price150: c.Price150, Price330: c.Price330, Price500: c.Price500}.Cups()
}

const catalogCols = `id, brewery, name, style, category, abv, ibu, ut_rating, ut_count,
	tags, poster_url, capacity_ml, cost_cents, price_150, price_330, price_500,
	created_at, updated_at`

func scanCatalogBeer(row pgx.Row) (CatalogBeer, error) {
	var c CatalogBeer
	err := row.Scan(
		&c.ID, &c.Brewery, &c.Name, &c.Style, &c.Category,
		&c.ABV, &c.IBU, &c.UT, &c.UTCount, &c.Tags, &c.PosterURL,
		&c.CapacityML, &c.CostCents, &c.Price150, &c.Price330, &c.Price500,
		&c.CreatedAt, &c.UpdatedAt,
	)
	return c, err
}

func ListCatalogBeers(ctx context.Context, pool *pgxpool.Pool, q, cat string) ([]CatalogBeer, error) {
	q = strings.TrimSpace(q)
	cat = strings.TrimSpace(cat)
	rows, err := pool.Query(ctx, `select `+catalogCols+`
		from beer_catalog
		where ($1 = ''
			or strpos(lower(name), lower($1)) > 0
			or strpos(lower(brewery), lower($1)) > 0
			or strpos(lower(brewery || ' ' || name), lower($1)) > 0
			or strpos(lower(style), lower($1)) > 0
			or strpos(lower(array_to_string(tags, ' ')), lower($1)) > 0)
		  and ($2 = '' or category = $2)
		order by updated_at desc, id desc`, q, cat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CatalogBeer
	for rows.Next() {
		c, err := scanCatalogBeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func CountCatalogBeers(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `select count(*)::int from beer_catalog`).Scan(&n)
	return n, err
}

func GetCatalogBeer(ctx context.Context, pool *pgxpool.Pool, id int64) (CatalogBeer, error) {
	return scanCatalogBeer(pool.QueryRow(ctx, `select `+catalogCols+` from beer_catalog where id=$1`, id))
}

func PrepareCatalogBeer(c CatalogBeer) (CatalogBeer, error) {
	return PrepareCatalogBeerWithCategories(c, platform.DefaultBeerCategories)
}

func PrepareCatalogBeerWithCategories(c CatalogBeer, allowed []string, extraAllowed ...string) (CatalogBeer, error) {
	c.Brewery = strings.TrimSpace(c.Brewery)
	c.Name = strings.TrimSpace(c.Name)
	c.Style = strings.TrimSpace(c.Style)
	c.PosterURL = strings.TrimSpace(c.PosterURL)
	cat, err := platform.ValidateBeerFieldsWithCategories(c.Name, c.Category, c.PosterURL, c.ABV, c.UT, c.IBU, c.UTCount, c.Price150, c.Price330, c.Price500, 0, allowed, extraAllowed...)
	if err != nil {
		return c, err
	}
	if cat == "" {
		cat = platform.InferCategory(c.Style, allowed)
	}
	c.Category = cat
	if c.CapacityML <= 0 {
		c.CapacityML = DefaultCapacityML
	}
	if c.CostCents < 0 {
		return c, platform.UserErr("金额不能为负")
	}
	if c.Tags == nil {
		c.Tags = []string{}
	}
	return c, nil
}

func mapCatalogSaveErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return platform.UserErr("历史酒单里已有相同「厂牌 + 酒名」的记录，请直接编辑原记录")
	}
	return err
}

func SaveCatalogBeer(ctx context.Context, pool *pgxpool.Pool, c CatalogBeer) (int64, error) {
	return SaveCatalogBeerWithCategories(ctx, pool, c, platform.DefaultBeerCategories)
}

func SaveCatalogBeerWithCategories(ctx context.Context, pool *pgxpool.Pool, c CatalogBeer, allowed []string, extraAllowed ...string) (int64, error) {
	c, err := PrepareCatalogBeerWithCategories(c, allowed, extraAllowed...)
	if err != nil {
		return 0, err
	}
	if c.ID == 0 {
		err := pool.QueryRow(ctx, `insert into beer_catalog
			(brewery, name, style, category, abv, ibu, ut_rating, ut_count,
			 tags, poster_url, capacity_ml, cost_cents, price_150, price_330, price_500)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) returning id`,
			c.Brewery, c.Name, c.Style, c.Category, c.ABV, c.IBU, c.UT, c.UTCount,
			c.Tags, c.PosterURL, c.CapacityML, c.CostCents, c.Price150, c.Price330, c.Price500,
		).Scan(&c.ID)
		return c.ID, mapCatalogSaveErr(err)
	}
	tag, err := pool.Exec(ctx, `update beer_catalog set
		brewery=$2, name=$3, style=$4, category=$5, abv=$6, ibu=$7, ut_rating=$8, ut_count=$9,
		tags=$10, poster_url=$11, capacity_ml=$12, cost_cents=$13,
		price_150=$14, price_330=$15, price_500=$16, updated_at=now()
		where id=$1`,
		c.ID, c.Brewery, c.Name, c.Style, c.Category, c.ABV, c.IBU, c.UT, c.UTCount,
		c.Tags, c.PosterURL, c.CapacityML, c.CostCents, c.Price150, c.Price330, c.Price500)
	if err != nil {
		return 0, mapCatalogSaveErr(err)
	}
	if tag.RowsAffected() == 0 {
		return 0, platform.UserErr("历史酒款不存在")
	}
	return c.ID, nil
}

func DeleteCatalogBeer(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `delete from beer_catalog where id=$1`, id)
	return err
}

// UpsertCatalogFromBeer 在进货入库或保存酒款时把最新资料沉淀到历史酒单。
func UpsertCatalogFromBeer(ctx context.Context, q platform.Querier, b Beer, capacityML, costCents int, updateKegSpec bool) error {
	return UpsertCatalogFromBeerWithCategories(ctx, q, b, capacityML, costCents, updateKegSpec, platform.DefaultBeerCategories)
}

// UpsertCatalogFromBeerWithCategories 在沉淀历史酒单时使用门店当前分类表推断缺失分类。
func UpsertCatalogFromBeerWithCategories(ctx context.Context, q platform.Querier, b Beer, capacityML, costCents int, updateKegSpec bool, allowed []string) error {
	return UpsertCatalogFromBeerFull(ctx, q, b, capacityML, costCents, updateKegSpec, false, allowed)
}

// UpsertCatalogFromBeerFull 支持选择是否在编辑清空白酒标时同步清空 beer_catalog.poster_url。
func UpsertCatalogFromBeerFull(ctx context.Context, q platform.Querier, b Beer, capacityML, costCents int, updateKegSpec, syncEmptyPoster bool, allowed []string) error {
	brewery := strings.TrimSpace(b.Brewery)
	name := strings.TrimSpace(b.Name)
	if name == "" {
		return nil
	}
	style := strings.TrimSpace(b.Style)
	cat := strings.TrimSpace(b.Category)
	if cat == "" {
		cat = platform.InferCategory(style, allowed)
	}
	poster := strings.TrimSpace(b.PosterURL)
	if !platform.ValidPosterURL(poster) {
		poster = ""
	}
	tags := b.Tags
	if tags == nil {
		tags = []string{}
	}
	capVal := capacityML
	if capVal <= 0 {
		capVal = DefaultCapacityML
	}
	costVal := max(costCents, 0)

	_, err := q.Exec(ctx, `insert into beer_catalog
		(brewery, name, style, category, abv, ibu, ut_rating, ut_count,
		 tags, poster_url, capacity_ml, cost_cents, price_150, price_330, price_500)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		on conflict (lower(btrim(brewery)), lower(btrim(name))) do update set
			brewery     = excluded.brewery,
			name        = excluded.name,
			style       = case when excluded.style <> '' then excluded.style else beer_catalog.style end,
			category    = case when excluded.category <> '' then excluded.category else beer_catalog.category end,
			abv         = coalesce(excluded.abv, beer_catalog.abv),
			ibu         = coalesce(excluded.ibu, beer_catalog.ibu),
			ut_rating   = coalesce(excluded.ut_rating, beer_catalog.ut_rating),
			ut_count    = coalesce(excluded.ut_count, beer_catalog.ut_count),
			tags        = case when cardinality(excluded.tags) > 0 then excluded.tags else beer_catalog.tags end,
			poster_url  = case when $17 or excluded.poster_url <> '' then excluded.poster_url else beer_catalog.poster_url end,
			capacity_ml = case when $16 and excluded.capacity_ml > 0 then excluded.capacity_ml else beer_catalog.capacity_ml end,
			cost_cents  = case when $16 then excluded.cost_cents else beer_catalog.cost_cents end,
			price_150   = case when excluded.price_150 > 0 or excluded.price_330 > 0 or excluded.price_500 > 0 then excluded.price_150 else beer_catalog.price_150 end,
			price_330   = case when excluded.price_150 > 0 or excluded.price_330 > 0 or excluded.price_500 > 0 then excluded.price_330 else beer_catalog.price_330 end,
			price_500   = case when excluded.price_150 > 0 or excluded.price_330 > 0 or excluded.price_500 > 0 then excluded.price_500 else beer_catalog.price_500 end,
			updated_at  = now()`,
		brewery, name, style, cat, b.ABV, b.IBU, b.UT, b.UTCount,
		tags, poster, capVal, costVal, max(b.Price150, 0), max(b.Price330, 0), max(b.Price500, 0),
		updateKegSpec, syncEmptyPoster,
	)
	return err
}

// ---------- 导入落库（防 23505 竞态） ----------

type PosterPayload struct {
	Filename string
	Data     []byte
}

type ImportFieldMask struct {
	Brewery    bool
	Style      bool
	Category   bool
	ABV        bool
	IBU        bool
	UT         bool
	UTCount    bool
	Tags       bool
	PosterURL  bool
	CapacityML bool
	CostCents  bool
	Price150   bool
	Price330   bool
	Price500   bool
}

type ImportItem struct {
	Beer   CatalogBeer
	Mask   ImportFieldMask
	Poster *PosterPayload
}

type ImportResult struct {
	Inserted int
	Updated  int
	Skipped  int
}

func ApplyCatalogMerge(dst CatalogBeer, src ImportItem) CatalogBeer {
	return ApplyCatalogMergeWithCategories(dst, src, platform.DefaultBeerCategories)
}

func ApplyCatalogMergeWithCategories(dst CatalogBeer, src ImportItem, allowed []string) CatalogBeer {
	if src.Mask.Brewery {
		dst.Brewery = src.Beer.Brewery
	}
	dst.Name = src.Beer.Name
	if src.Mask.Style {
		dst.Style = src.Beer.Style
	}
	if src.Mask.Category {
		dst.Category = src.Beer.Category
	}
	if dst.Category == "" {
		dst.Category = platform.InferCategory(dst.Style, allowed)
	}
	if src.Mask.ABV {
		dst.ABV = src.Beer.ABV
	}
	if src.Mask.IBU {
		dst.IBU = src.Beer.IBU
	}
	if src.Mask.UT {
		dst.UT = src.Beer.UT
	}
	if src.Mask.UTCount {
		dst.UTCount = src.Beer.UTCount
	}
	if src.Mask.Tags {
		dst.Tags = src.Beer.Tags
	}
	if dst.Tags == nil {
		dst.Tags = []string{}
	}
	if src.Mask.PosterURL {
		dst.PosterURL = src.Beer.PosterURL
	}
	if src.Mask.CapacityML && src.Beer.CapacityML > 0 {
		dst.CapacityML = src.Beer.CapacityML
	}
	if dst.CapacityML <= 0 {
		dst.CapacityML = DefaultCapacityML
	}
	if src.Mask.CostCents {
		dst.CostCents = src.Beer.CostCents
	}
	if src.Mask.Price150 {
		dst.Price150 = src.Beer.Price150
	}
	if src.Mask.Price330 {
		dst.Price330 = src.Beer.Price330
	}
	if src.Mask.Price500 {
		dst.Price500 = src.Beer.Price500
	}
	return dst
}

func findCatalogForUpdateTx(ctx context.Context, tx pgx.Tx, brewery, name string) (CatalogBeer, error) {
	return scanCatalogBeer(tx.QueryRow(ctx, `select `+catalogCols+` from beer_catalog
		where lower(btrim(brewery)) = lower(btrim($1))
		  and lower(btrim(name))    = lower(btrim($2))
		for update`, brewery, name))
}

func insertCatalogDoNothingTx(ctx context.Context, tx pgx.Tx, c CatalogBeer) (pgconn.CommandTag, error) {
	return tx.Exec(ctx, `insert into beer_catalog
		(brewery, name, style, category, abv, ibu, ut_rating, ut_count,
		 tags, poster_url, capacity_ml, cost_cents, price_150, price_330, price_500)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		on conflict (lower(btrim(brewery)), lower(btrim(name))) do nothing`,
		c.Brewery, c.Name, c.Style, c.Category, c.ABV, c.IBU, c.UT, c.UTCount,
		c.Tags, c.PosterURL, c.CapacityML, c.CostCents, c.Price150, c.Price330, c.Price500)
}

func ImportCatalogBeers(ctx context.Context, pool *pgxpool.Pool, items []ImportItem, mode string) (ImportResult, error) {
	return ImportCatalogBeersWithCategories(ctx, pool, items, mode, platform.DefaultBeerCategories)
}

func ImportCatalogBeersWithCategories(ctx context.Context, pool *pgxpool.Pool, items []ImportItem, mode string, allowed []string) (ImportResult, error) {
	return ImportCatalogBeersWithPosters(ctx, pool, nil, items, mode, allowed)
}

// ImportCatalogBeersWithPosters 在单事务导入历史酒单的同时落盘压缩包内酒标并在失败/跳过/替换时自动回收孤儿图片。
func ImportCatalogBeersWithPosters(ctx context.Context, pool *pgxpool.Pool, store *PosterStore, items []ImportItem, mode string, allowed []string) (ImportResult, error) {
	if mode == "" {
		mode = "merge"
	}
	if mode != "merge" && mode != "skip" {
		return ImportResult{}, platform.UserErr("不支持的导入模式")
	}

	work := slices.Clone(items)
	for _, it := range work {
		if strings.TrimSpace(it.Beer.Name) == "" {
			return ImportResult{}, platform.UserErr("酒款名称不能为空")
		}
	}

	var createdFiles []string
	createdSet := make(map[string]bool)
	committed := false
	defer func() {
		if !committed && store != nil {
			for _, u := range createdFiles {
				_ = store.Delete(u)
			}
		}
	}()

	if store != nil {
		for i := range work {
			it := &work[i]
			switch {
			case it.Poster != nil && len(it.Poster.Data) > 0:
				savedURL, err := store.SaveBytes(it.Poster.Data)
				if err != nil {
					return ImportResult{}, err
				}
				createdFiles = append(createdFiles, savedURL)
				createdSet[savedURL] = true
				it.Beer.PosterURL = savedURL
				it.Mask.PosterURL = true
			case strings.HasPrefix(strings.ToLower(strings.TrimSpace(it.Beer.PosterURL)), "http"):
				if savedURL, err := store.SaveFromURL(ctx, it.Beer.PosterURL); err == nil && savedURL != "" {
					createdFiles = append(createdFiles, savedURL)
					createdSet[savedURL] = true
					it.Beer.PosterURL = savedURL
					it.Mask.PosterURL = true
				}
			case IsLocalPosterURL(it.Beer.PosterURL) && !store.Exists(it.Beer.PosterURL):
				it.Beer.PosterURL = ""
			}
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	defer tx.Rollback(ctx)

	var res ImportResult
	var cleanupCandidates []string
	for _, it := range work {
		if strings.TrimSpace(it.Beer.Name) == "" {
			return ImportResult{}, platform.UserErr("酒款名称不能为空")
		}
		if mode == "skip" {
			c, err := PrepareCatalogBeerWithCategories(ApplyCatalogMergeWithCategories(CatalogBeer{CapacityML: DefaultCapacityML, Tags: []string{}}, it, allowed), allowed)
			if err != nil {
				return ImportResult{}, err
			}
			tag, err := insertCatalogDoNothingTx(ctx, tx, c)
			if err != nil {
				return ImportResult{}, err
			}
			if tag.RowsAffected() == 0 {
				if createdSet[c.PosterURL] {
					cleanupCandidates = append(cleanupCandidates, c.PosterURL)
				}
				res.Skipped++
			} else {
				res.Inserted++
			}
			continue
		}

		existing, err := findCatalogForUpdateTx(ctx, tx, it.Beer.Brewery, it.Beer.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			c, perr := PrepareCatalogBeerWithCategories(ApplyCatalogMergeWithCategories(CatalogBeer{CapacityML: DefaultCapacityML, Tags: []string{}}, it, allowed), allowed)
			if perr != nil {
				return ImportResult{}, perr
			}
			tag, ierr := insertCatalogDoNothingTx(ctx, tx, c)
			if ierr != nil {
				return ImportResult{}, ierr
			}
			if tag.RowsAffected() > 0 {
				res.Inserted++
				continue
			}
			existing, err = findCatalogForUpdateTx(ctx, tx, it.Beer.Brewery, it.Beer.Name)
		}
		if err != nil {
			return ImportResult{}, err
		}
		merged, err := PrepareCatalogBeerWithCategories(ApplyCatalogMergeWithCategories(existing, it, allowed), allowed, existing.Category)
		if err != nil {
			return ImportResult{}, err
		}
		if _, err := tx.Exec(ctx, `update beer_catalog set
			brewery=$2, name=$3, style=$4, category=$5, abv=$6, ibu=$7, ut_rating=$8, ut_count=$9,
			tags=$10, poster_url=$11, capacity_ml=$12, cost_cents=$13,
			price_150=$14, price_330=$15, price_500=$16, updated_at=now()
			where id=$1`,
			existing.ID, merged.Brewery, merged.Name, merged.Style, merged.Category,
			merged.ABV, merged.IBU, merged.UT, merged.UTCount, merged.Tags, merged.PosterURL,
			merged.CapacityML, merged.CostCents, merged.Price150, merged.Price330, merged.Price500,
		); err != nil {
			return ImportResult{}, err
		}
		if existing.PosterURL != merged.PosterURL && IsLocalPosterURL(existing.PosterURL) {
			cleanupCandidates = append(cleanupCandidates, existing.PosterURL)
		}
		res.Updated++
	}

	if err := tx.Commit(ctx); err != nil {
		return ImportResult{}, err
	}
	committed = true
	CleanupUnusedPosters(ctx, pool, store, cleanupCandidates...)
	return res, nil
}
