package inventory

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

// BeerRow 是酒单列表的一行：酒款 + 当前在售的那一桶（没有则 nil）+ 上次结桶复盘。
type BeerRow struct {
	catalog.Beer
	Keg     *Keg
	LastKeg *Keg
	Stock   int // 进了货但还没开的桶数
}

type BeerListPage struct {
	ui.Layout
	Rows    []BeerRow
	NextNo  int
	Pricing settings.PricingConfig
}

func (p BeerListPage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

type BeerFormPage struct {
	ui.Layout
	Beer      catalog.Beer
	Keg       Keg
	CostInput string
	Pricing   settings.PricingConfig
}

func (p BeerFormPage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

type PurchasePage struct {
	ui.Layout
	Beer      catalog.Beer
	Keg       Keg
	KegCount  int
	CostInput string
	Pricing   settings.PricingConfig
}

func (p PurchasePage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

func (s *Service) BeerList(w http.ResponseWriter, r *http.Request) {
	beers, err := catalog.ListBeers(r.Context(), s.pool, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active, err := ListKegs(r.Context(), s.pool, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stock, err := StockCounts(r.Context(), s.pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lastFinished, err := LatestFinishedKegs(r.Context(), s.pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byBeer := make(map[int64]*Keg, len(active))
	for i := range active {
		byBeer[active[i].BeerID] = &active[i]
	}
	rows := make([]BeerRow, 0, len(beers))
	used := make(map[int]bool, len(beers))
	for _, b := range beers {
		rows = append(rows, BeerRow{Beer: b, Keg: byBeer[b.ID], LastKeg: lastFinished[b.ID], Stock: stock[b.ID]})
		if n, err := strconv.Atoi(strings.TrimSpace(b.No)); err == nil && n > 0 {
			used[n] = true
		}
	}
	next := 1
	for used[next] {
		next++
	}
	s.renderer.Render(w, r, "admin/beers.html", &BeerListPage{
		Layout: ui.Layout{
			Title: "酒单 · 桶装",
			Nav:   "beers",
			Msg:   r.URL.Query().Get("msg"),
			Err:   r.URL.Query().Get("err"),
		},
		Rows:    rows,
		NextNo:  next,
		Pricing: s.currentPricing(),
	})
}

func (s *Service) PurchasePage(w http.ResponseWriter, r *http.Request) {
	data := &PurchasePage{
		Layout: ui.Layout{
			Title: "新增酒款",
			Nav:   "beers",
			Msg:   r.URL.Query().Get("msg"),
			Err:   r.URL.Query().Get("err"),
		},
		Keg:      Keg{CapacityML: catalog.DefaultCapacityML},
		KegCount: 1,
		Pricing:  s.currentPricing(),
	}
	if rawID := strings.TrimSpace(r.URL.Query().Get("catalog_id")); rawID != "" {
		if cid, err := strconv.ParseInt(rawID, 10, 64); err == nil && cid > 0 && s.pool != nil {
			c, err := catalog.GetCatalogBeer(r.Context(), s.pool, cid)
			switch {
			case err == nil:
				data.Beer = catalog.Beer{
					Brewery:   c.Brewery,
					Name:      c.Name,
					Style:     c.Style,
					Category:  c.Category,
					ABV:       c.ABV,
					IBU:       c.IBU,
					Price150:  c.Price150,
					Price330:  c.Price330,
					Price500:  c.Price500,
					UT:        c.UT,
					UTCount:   c.UTCount,
					Tags:      c.Tags,
					PosterURL: c.PosterURL,
				}
				if c.CapacityML > 0 {
					data.Keg.CapacityML = c.CapacityML
				}
				data.CostInput = c.CostInput()
				data.Msg = fmt.Sprintf("已从历史酒单带入「%s」的资料，核对进货桶数与价格后即可入库", platform.BeerSubject(c.Brewery, c.Name))
			case errors.Is(err, pgx.ErrNoRows):
				data.Err = "指定的历史酒款不存在，可重新搜索或手动填写"
			}
		}
	}
	s.renderer.Render(w, r, "admin/purchase.html", data)
}

func (s *Service) PurchaseBeer(w http.ResponseWriter, r *http.Request) {
	rawCost := strings.TrimSpace(r.FormValue("cost"))
	rawKegs := strings.TrimSpace(r.FormValue("kegs"))
	rawCap := strings.TrimSpace(r.FormValue("capacity_ml"))

	cats := s.currentCategories()
	parsed, formErr := platform.ParseBeerFormWithCategories(r, cats)
	b := catalog.Beer{
		Brewery:   parsed.Brewery,
		Name:      parsed.Name,
		Style:     parsed.Style,
		Category:  parsed.Category,
		PosterURL: parsed.PosterURL,
		ABV:       parsed.ABV,
		IBU:       parsed.IBU,
		UT:        parsed.UT,
		UTCount:   parsed.UTCount,
		Price150:  parsed.Price150,
		Price330:  parsed.Price330,
		Price500:  parsed.Price500,
		Tags:      parsed.Tags,
		OnScreen:  false, // 进货只是入库，先不上屏
	}

	count := 0
	countPtr, countErr := platform.ParseStrictInt(rawKegs, "进货桶数", 1, 100)
	if countPtr != nil {
		count = *countPtr
	}
	capacity := 0
	capPtr, capErr := platform.ParseStrictInt(rawCap, "每桶容量", 1, math.MaxInt32)
	if capPtr != nil {
		capacity = *capPtr
	}
	costCents, costErr := platform.MoneyCents(rawCost)

	pricing := s.currentPricing()
	fail := func(status int, msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.renderer.Render(w, r, "admin/purchase.html", &PurchasePage{
			Layout: ui.Layout{
				Title: "新增酒款",
				Nav:   "beers",
				Err:   msg,
			},
			Beer:      b,
			Keg:       Keg{CapacityML: capacity},
			KegCount:  count,
			CostInput: rawCost,
			Pricing:   pricing,
		})
	}
	switch {
	case formErr != nil:
		fail(http.StatusBadRequest, formErr.Error())
		return
	case countErr != nil || count < 1 || count > 100:
		fail(http.StatusBadRequest, "进货桶数需在 1 到 100 之间")
		return
	case capErr != nil:
		fail(http.StatusBadRequest, capErr.Error())
		return
	case capacity <= 0:
		fail(http.StatusBadRequest, "每桶容量必须大于 0")
		return
	case costErr != nil:
		fail(http.StatusBadRequest, costErr.Error())
		return
	}
	beerID, err := PurchaseBeerTxWithCategories(r.Context(), s.pool, b, count, capacity, costCents, cats)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			fail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("purchase beer: %v", err)
		fail(http.StatusInternalServerError, "入库失败，请稍后重试")
		return
	}
	subject := platform.BeerSubject(b.Brewery, b.Name)
	s.publish(r, event.EventScreenChanged, subject, map[string]any{
		"action":  "beer.purchased",
		"beer_id": beerID,
	})
	s.publish(r, event.EventBeerPurchased, subject, map[string]any{
		"beer_id":     beerID,
		"count":       count,
		"capacity_ml": capacity,
		"cost_cents":  costCents,
	})
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已入库，去列表里上架"), http.StatusSeeOther)
}

func (s *Service) TapBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	res, err := TapBeerResult(r.Context(), s.pool, id, r.FormValue("no"),
		platform.IntValue(r.FormValue("capacity_ml")), platform.PriceCents(r.FormValue("cost")))
	if err != nil {
		http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	subject := platform.BeerSubject(res.Brewery, res.Name)
	s.publish(r, event.EventScreenChanged, subject, map[string]any{
		"action":  "beer.tapped",
		"beer_id": res.BeerID,
		"keg_id":  res.KegID,
		"no":      res.No,
	})
	s.publish(r, event.EventBeerTapped, subject, map[string]any{
		"beer_id":    res.BeerID,
		"keg_id":     res.KegID,
		"no":         res.No,
		"opened_new": res.OpenedNew,
	})
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已上架"), http.StatusSeeOther)
}

func (s *Service) UntapBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kegID, err := UntapBeerResult(r.Context(), s.pool, id)
	if err != nil {
		http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	pricing := s.currentPricing()
	msg := "已下架，本桶记停售"
	subject := ""
	var finishedPayload map[string]any
	if kegID > 0 {
		if k, err := GetKeg(r.Context(), s.pool, kegID); err == nil {
			msg = k.UntapSummaryTextWithConfig(pricing)
			subject = platform.BeerSubject(k.Brewery, k.BeerName)
			econ := k.EconomicsWithConfig(nil, pricing)
			finishedPayload = map[string]any{
				"keg_id":        k.ID,
				"beer_id":       k.BeerID,
				"revenue_cents": k.RevenueCents,
				"cost_cents":    k.CostCents,
				"profit_cents":  k.ProfitCents(),
				"verdict":       econ.Verdict,
				"yield_pct":     econ.ActualYieldPct,
			}
		}
	}
	s.publish(r, event.EventScreenChanged, subject, map[string]any{
		"action":  "beer.untapped",
		"beer_id": id,
		"keg_id":  kegID,
	})
	if finishedPayload != nil {
		s.publish(r, event.EventKegFinished, subject, finishedPayload)
	}
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg(msg), http.StatusSeeOther)
}

func (s *Service) EditBeer(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := catalog.GetBeer(r.Context(), s.pool, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	keg, costInput := LatestBeerKegSpec(r.Context(), s.pool, b)
	s.renderer.Render(w, r, "admin/beer_form.html", &BeerFormPage{
		Layout: ui.Layout{
			Title: "编辑酒款",
			Nav:   "beers",
		},
		Beer:      b,
		Keg:       keg,
		CostInput: costInput,
		Pricing:   s.currentPricing(),
	})
}

func (s *Service) SaveBeer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单格式不正确", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var beerID int64
	if id := r.FormValue("id"); id != "" {
		beerID, _ = strconv.ParseInt(id, 10, 64)
	}
	cats := s.currentCategories()
	var extraAllowed []string
	if beerID > 0 && s.pool != nil {
		if ex, err := catalog.GetBeer(ctx, s.pool, beerID); err == nil && ex.Category != "" {
			extraAllowed = append(extraAllowed, ex.Category)
		}
	}

	parsed, formErr := platform.ParseBeerFormWithCategories(r, cats, extraAllowed...)
	sortOrder, sortErr := platform.ParseSortOrder(r.FormValue("sort_order"))
	b := catalog.Beer{
		ID:        beerID,
		No:        strings.TrimSpace(r.FormValue("no")),
		Brewery:   parsed.Brewery,
		Name:      parsed.Name,
		Style:     parsed.Style,
		Category:  parsed.Category,
		ABV:       parsed.ABV,
		IBU:       parsed.IBU,
		Price150:  parsed.Price150,
		Price330:  parsed.Price330,
		Price500:  parsed.Price500,
		UT:        parsed.UT,
		UTCount:   parsed.UTCount,
		Tags:      parsed.Tags,
		SalesTags: platform.SplitList(r.FormValue("sales_tags")),
		PosterURL: parsed.PosterURL,
		OnScreen:  r.FormValue("on_screen") != "",
		SortOrder: sortOrder,
	}

	pricing := s.currentPricing()
	renderFail := func(status int, msg string) {
		keg, costInput := LatestBeerKegSpec(ctx, s.pool, b)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		s.renderer.Render(w, r, "admin/beer_form.html", &BeerFormPage{
			Layout: ui.Layout{
				Title: "编辑酒款",
				Nav:   "beers",
				Err:   msg,
			},
			Beer:      b,
			Keg:       keg,
			CostInput: costInput,
			Pricing:   pricing,
		})
	}
	if formErr != nil {
		renderFail(http.StatusBadRequest, formErr.Error())
		return
	}
	if sortErr != nil {
		renderFail(http.StatusBadRequest, sortErr.Error())
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		log.Printf("save beer begin (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	defer tx.Rollback(ctx)
	savedID, err := catalog.SaveBeerWithCategories(ctx, tx, b, cats, extraAllowed...)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			renderFail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("save beer (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	if err := catalog.UpsertCatalogFromBeerWithCategories(ctx, tx, b, 0, 0, false, cats); err != nil {
		log.Printf("save beer catalog sync (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("save beer commit (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	s.publish(r, event.EventScreenChanged, platform.BeerSubject(b.Brewery, b.Name), map[string]any{
		"action":  "beer.saved",
		"beer_id": savedID,
	})
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已保存"), http.StatusSeeOther)
}

func (s *Service) ToggleBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	on, err := catalog.ToggleBeer(r.Context(), s.pool, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.publish(r, event.EventScreenChanged, "", map[string]any{
		"action":    "beer.toggled",
		"beer_id":   id,
		"on_screen": on,
	})
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已更新上屏状态"), http.StatusSeeOther)
}

func (s *Service) DeleteBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := catalog.DeleteBeer(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			http.Redirect(w, r, "/admin/beers?err="+platform.URLMsg(msg), http.StatusSeeOther)
			return
		}
		log.Printf("delete beer (%d): %v", id, err)
		http.Redirect(w, r, "/admin/beers?err="+platform.URLMsg("删除失败，请稍后重试"), http.StatusSeeOther)
		return
	}
	s.publish(r, event.EventScreenChanged, "", map[string]any{
		"action":  "beer.deleted",
		"beer_id": id,
	})
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已删除"), http.StatusSeeOther)
}
