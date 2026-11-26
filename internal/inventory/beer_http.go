package inventory

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

func parseBeerRequestForm(w http.ResponseWriter, r *http.Request) (func(), error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(ct, "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, catalog.MaxPosterBytes+2<<20)
		if err := r.ParseMultipartForm(catalog.MaxPosterBytes); err != nil {
			return func() {}, platform.UserErr("上传图片过大或表单格式不正确（酒标上限 5MB）")
		}
		return func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}, nil
	}
	if err := r.ParseForm(); err != nil {
		return func() {}, platform.UserErr("表单格式不正确")
	}
	return func() {}, nil
}

func (s *Service) resolveSubmittedPoster(ctx context.Context, r *http.Request, rawURL string) (finalURL string, createdLocal string, err error) {
	file, _, ferr := r.FormFile("poster_file")
	switch {
	case ferr == nil:
		defer file.Close()
		if s.posters == nil {
			return "", "", platform.UserErr("酒标存储未初始化")
		}
		savedURL, saveErr := s.posters.SaveReader(file)
		if saveErr != nil {
			return "", "", saveErr
		}
		return savedURL, savedURL, nil
	case errors.Is(ferr, http.ErrMissingFile), errors.Is(ferr, http.ErrNotMultipart):
	default:
		return "", "", platform.UserErr("读取上传酒标失败，请重试")
	}

	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", nil
	}
	lower := strings.ToLower(rawURL)
	if (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) && s.posters != nil {
		savedURL, fetchErr := s.posters.SaveFromURL(ctx, rawURL)
		if fetchErr != nil {
			return "", "", fetchErr
		}
		return savedURL, savedURL, nil
	}
	if catalog.IsLocalPosterURL(rawURL) && s.posters != nil && !s.posters.Exists(rawURL) {
		return "", "", platform.UserErr("指定的本地酒标文件不存在，请重新上传或粘贴图片地址")
	}
	return rawURL, "", nil
}

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
		log.Printf("beer list: %v", err)
		http.Error(w, "加载酒单失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	active, err := ListKegs(r.Context(), s.pool, true)
	if err != nil {
		log.Printf("beer list active kegs: %v", err)
		http.Error(w, "加载酒单失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	stock, err := StockCounts(r.Context(), s.pool)
	if err != nil {
		log.Printf("beer list stock counts: %v", err)
		http.Error(w, "加载酒单失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	lastFinished, err := LatestFinishedKegs(r.Context(), s.pool)
	if err != nil {
		log.Printf("beer list last finished: %v", err)
		http.Error(w, "加载酒单失败，请稍后重试", http.StatusInternalServerError)
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
		},
		Keg:      Keg{CapacityML: catalog.DefaultCapacityML},
		KegCount: 1,
		Pricing:  s.currentPricing(),
	}
	s.renderer.Render(w, r, "admin/purchase.html", data)
}

func (s *Service) PurchaseBeer(w http.ResponseWriter, r *http.Request) {
	cleanupForm, reqErr := parseBeerRequestForm(w, r)
	defer cleanupForm()

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
		s.renderer.RenderStatus(w, r, status, "admin/purchase.html", &PurchasePage{
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
	case reqErr != nil:
		fail(http.StatusBadRequest, reqErr.Error())
		return
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

	ctx := r.Context()
	finalPoster, createdLocal, posterErr := s.resolveSubmittedPoster(ctx, r, parsed.PosterURL)
	if posterErr != nil {
		if msg, ok := platform.UserErrorMessage(posterErr); ok {
			fail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("purchase poster: %v", posterErr)
		fail(http.StatusInternalServerError, "保存酒标失败，请稍后重试")
		return
	}
	b.PosterURL = finalPoster
	committed := false
	defer func() {
		if !committed && createdLocal != "" && s.posters != nil {
			_ = s.posters.Delete(createdLocal)
		}
	}()

	beerID, oldPosters, err := PurchaseBeerTxWithPosterCleanup(ctx, s.pool, b, count, capacity, costCents, cats)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			fail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("purchase beer: %v", err)
		fail(http.StatusInternalServerError, "入库失败，请稍后重试")
		return
	}
	committed = true
	catalog.CleanupUnusedPosters(ctx, s.pool, s.posters, oldPosters...)

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
	platform.RedirectMsg(w, r, "/admin/beers", "已入库，去列表里上架")
}

func (s *Service) TapBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	capacityML := 0
	if rawCap := strings.TrimSpace(r.FormValue("capacity_ml")); rawCap != "" {
		capPtr, capErr := platform.ParseStrictInt(rawCap, "每桶容量", 1, math.MaxInt32)
		if capErr != nil {
			platform.RedirectErr(w, r, "/admin/beers", capErr.Error())
			return
		}
		if capPtr != nil {
			capacityML = *capPtr
		}
	}
	costCents, costErr := platform.MoneyCents(r.FormValue("cost"))
	if costErr != nil {
		platform.RedirectErr(w, r, "/admin/beers", costErr.Error())
		return
	}
	res, err := TapBeerResult(r.Context(), s.pool, id, r.FormValue("no"), capacityML, costCents)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/beers", msg)
			return
		}
		log.Printf("tap beer (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/beers", "上架失败，请稍后重试")
		return
	}
	subject := platform.BeerSubject(res.Brewery, res.Name)
	s.publish(r, event.EventScreenChanged, subject, map[string]any{
		"action":  "beer.tapped",
		"beer_id": res.BeerID,
		"keg_id":  res.KegID,
		"no":      res.No,
	})
	if res.OpenedNew {
		s.publish(r, event.EventBeerTapped, subject, map[string]any{
			"beer_id":    res.BeerID,
			"keg_id":     res.KegID,
			"no":         res.No,
			"opened_new": true,
		})
	}
	platform.RedirectMsg(w, r, "/admin/beers", "已上架")
}

func (s *Service) UntapBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kegID, err := UntapBeerResult(r.Context(), s.pool, id)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/beers", msg)
			return
		}
		log.Printf("untap beer (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/beers", "下架失败，请稍后重试")
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
	platform.RedirectMsg(w, r, "/admin/beers", msg)
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
	cleanupForm, reqErr := parseBeerRequestForm(w, r)
	defer cleanupForm()

	ctx := r.Context()
	var beerID int64
	if id := r.FormValue("id"); id != "" {
		beerID, _ = strconv.ParseInt(id, 10, 64)
	}
	cats := s.currentCategories()
	var extraAllowed []string
	var oldPosters []string
	if beerID > 0 && s.pool != nil {
		if ex, err := catalog.GetBeer(ctx, s.pool, beerID); err == nil {
			if ex.Category != "" {
				extraAllowed = append(extraAllowed, ex.Category)
			}
			if ex.PosterURL != "" {
				oldPosters = append(oldPosters, ex.PosterURL)
			}
			if prevCat := catalog.LookupCatalogPoster(ctx, s.pool, ex.Brewery, ex.Name); prevCat != "" {
				oldPosters = append(oldPosters, prevCat)
			}
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
		s.renderer.RenderStatus(w, r, status, "admin/beer_form.html", &BeerFormPage{
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
	if reqErr != nil {
		renderFail(http.StatusBadRequest, reqErr.Error())
		return
	}
	if formErr != nil {
		renderFail(http.StatusBadRequest, formErr.Error())
		return
	}
	if sortErr != nil {
		renderFail(http.StatusBadRequest, sortErr.Error())
		return
	}

	finalPoster, createdLocal, posterErr := s.resolveSubmittedPoster(ctx, r, parsed.PosterURL)
	if posterErr != nil {
		if msg, ok := platform.UserErrorMessage(posterErr); ok {
			renderFail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("save beer poster (%d): %v", b.ID, posterErr)
		renderFail(http.StatusInternalServerError, "保存酒标失败，请稍后重试")
		return
	}
	b.PosterURL = finalPoster
	committed := false
	defer func() {
		if !committed && createdLocal != "" && s.posters != nil {
			_ = s.posters.Delete(createdLocal)
		}
	}()

	if prevCat := catalog.LookupCatalogPoster(ctx, s.pool, b.Brewery, b.Name); prevCat != "" {
		oldPosters = append(oldPosters, prevCat)
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
	if err := catalog.UpsertCatalogFromBeerFull(ctx, tx, b, 0, 0, false, true, cats); err != nil {
		log.Printf("save beer catalog sync (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("save beer commit (%d): %v", b.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	committed = true
	cleanupCandidates := make([]string, 0, len(oldPosters))
	for _, oldURL := range oldPosters {
		if oldURL != b.PosterURL {
			cleanupCandidates = append(cleanupCandidates, oldURL)
		}
	}
	catalog.CleanupUnusedPosters(ctx, s.pool, s.posters, cleanupCandidates...)

	s.publish(r, event.EventScreenChanged, platform.BeerSubject(b.Brewery, b.Name), map[string]any{
		"action":  "beer.saved",
		"beer_id": savedID,
	})
	platform.RedirectMsg(w, r, "/admin/beers", "已保存")
}

func (s *Service) ToggleBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	on, err := catalog.ToggleBeer(r.Context(), s.pool, id)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/beers", msg)
			return
		}
		log.Printf("toggle beer (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/beers", "操作失败，请稍后重试")
		return
	}
	s.publish(r, event.EventScreenChanged, "", map[string]any{
		"action":    "beer.toggled",
		"beer_id":   id,
		"on_screen": on,
	})
	platform.RedirectMsg(w, r, "/admin/beers", "已更新上屏状态")
}

func (s *Service) DeleteBeerHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var oldPoster string
	if s.pool != nil {
		if b, err := catalog.GetBeer(r.Context(), s.pool, id); err == nil {
			oldPoster = b.PosterURL
		}
	}
	if err := catalog.DeleteBeer(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/beers", msg)
			return
		}
		log.Printf("delete beer (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/beers", "删除失败，请稍后重试")
		return
	}
	catalog.CleanupUnusedPosters(r.Context(), s.pool, s.posters, oldPoster)
	s.publish(r, event.EventScreenChanged, "", map[string]any{
		"action":  "beer.deleted",
		"beer_id": id,
	})
	platform.RedirectMsg(w, r, "/admin/beers", "已删除")
}
