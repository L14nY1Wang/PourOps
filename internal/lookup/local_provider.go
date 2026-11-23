package lookup

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/platform"
)

// LocalCatalogProvider 从门店自建 beer_catalog 表提供最高优先级的历史酒款检索与回填。
type LocalCatalogProvider struct {
	pool *pgxpool.Pool
}

func NewLocalCatalogProvider(pool *pgxpool.Pool) *LocalCatalogProvider {
	return &LocalCatalogProvider{pool: pool}
}

func (p *LocalCatalogProvider) Name() string             { return "catalog" }
func (p *LocalCatalogProvider) Label() string            { return "本店历史酒单" }
func (p *LocalCatalogProvider) Enabled() bool            { return true }
func (p *LocalCatalogProvider) SupportsEmptyQuery() bool { return true }

func (p *LocalCatalogProvider) Search(ctx context.Context, q string, limit int) ([]Candidate, error) {
	if p == nil || p.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 12
	}

	var (
		rows pgx.Rows
		err  error
	)
	if q == "" {
		rows, err = p.pool.Query(ctx, `
			select id, brewery, name, style, ut_rating, poster_url,
			       (price_150 > 0 or price_330 > 0 or price_500 > 0) as has_pricing
			from beer_catalog
			order by updated_at desc, id desc
			limit $1`, limit)
	} else {
		rows, err = p.pool.Query(ctx, `
			select id, brewery, name, style, ut_rating, poster_url,
			       (price_150 > 0 or price_330 > 0 or price_500 > 0) as has_pricing
			from beer_catalog
			where strpos(lower(name), lower($1)) > 0
			   or strpos(lower(brewery), lower($1)) > 0
			   or strpos(lower(brewery || ' ' || name), lower($1)) > 0
			   or strpos(lower(style), lower($1)) > 0
			   or strpos(lower(array_to_string(tags, ' ')), lower($1)) > 0
			order by
				case
					when lower(name) = lower($1) or lower(brewery || ' ' || name) = lower($1) then 0
					when starts_with(lower(name), lower($1)) or starts_with(lower(brewery || ' ' || name), lower($1)) then 1
					when strpos(lower(name), lower($1)) > 0 or strpos(lower(brewery), lower($1)) > 0 then 2
					else 3
				end,
				updated_at desc,
				id desc
			limit $2`, q, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var id int64
		var brewery, name, style, poster string
		var ut *float64
		var hasPricing bool
		if err := rows.Scan(&id, &brewery, &name, &style, &ut, &poster, &hasPricing); err != nil {
			return nil, err
		}
		rating := ""
		if ut != nil && *ut > 0 {
			rating = fmt.Sprintf("%.2f", *ut)
		}
		out = append(out, Candidate{
			Source:     "catalog",
			ID:         strconv.FormatInt(id, 10),
			Brewery:    brewery,
			Name:       name,
			Style:      style,
			Rating:     rating,
			PosterURL:  poster,
			HasPricing: hasPricing,
		})
	}
	return out, rows.Err()
}

func (p *LocalCatalogProvider) Detail(ctx context.Context, rawID string) (Detail, error) {
	beerID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || beerID <= 0 {
		return Detail{}, ErrInvalidID
	}
	if p == nil || p.pool == nil {
		return Detail{}, ErrNotFound
	}
	c, err := catalog.GetCatalogBeer(ctx, p.pool, beerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	cat := c.Category
	if cat == "" {
		cat = platform.ToneOf(c.Style)
	}
	capText := ""
	if c.CapacityML > 0 {
		capText = strconv.Itoa(c.CapacityML)
	}
	return Detail{
		Source:      "catalog",
		SourceLabel: "本店历史酒单",
		Brewery:     c.Brewery,
		Name:        c.Name,
		Style:       c.Style,
		Category:    cat,
		ABV:         c.ABVInput(),
		IBU:         c.IBUInput(),
		UTRating:    c.UTInput(),
		UTCount:     c.UTCountInput(),
		Tags:        c.TagsText(),
		PosterURL:   c.PosterURL,
		CapacityML:  capText,
		Cost:        c.CostInput(),
		Price150:    c.Price150Input(),
		Price330:    c.Price330Input(),
		Price500:    c.Price500Input(),
	}, nil
}
