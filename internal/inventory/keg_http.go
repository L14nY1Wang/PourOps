package inventory

import (
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

const kegPageSize = 50

type KegListPage struct {
	ui.Layout
	Kegs       []Keg
	KegSummary KegSummary
	Status     string
	Verdict    string
	Pricing    settings.PricingConfig
	Page       int
	TotalPages int
	PrevURL    string
	NextURL    string
}

func kegListURL(status, verdict string, page int) string {
	vals := url.Values{}
	if status != "" && status != "all" {
		vals.Set("status", status)
	}
	if verdict != "" && verdict != "all" {
		vals.Set("verdict", verdict)
	}
	if page > 1 {
		vals.Set("page", strconv.Itoa(page))
	}
	if len(vals) == 0 {
		return "/admin/kegs"
	}
	return "/admin/kegs?" + vals.Encode()
}

func parseOptionalTimeInput(raw, label string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	t := platform.ParseLocalTime(raw)
	if t == nil {
		return nil, platform.UserErr(label + "格式不对")
	}
	return t, nil
}

func (p KegListPage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

type KegDetailPage struct {
	ui.Layout
	Keg        Keg
	Beer       catalog.Beer
	KegPricing []CupPricingRow
	Pours      []Pour
	Pricing    settings.PricingConfig
}

func (p KegDetailPage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

func (p KegDetailPage) Econ() KegEconomics {
	return p.Keg.EconomicsWithConfig(p.PricingCfg())
}

func (p KegDetailPage) PricingRows() []CupPricingRow {
	if len(p.KegPricing) > 0 {
		return p.KegPricing
	}
	return p.Keg.CupPricingWithConfig(p.Beer, p.PricingCfg())
}

type KegFormPage struct {
	ui.Layout
	Keg   Keg
	Beers []catalog.Beer
}

func (s *Service) KegList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	allKegs, err := ListKegs(ctx, s.pool, false)
	if err != nil {
		log.Printf("keg list: %v", err)
		http.Error(w, "加载酒桶列表失败，请稍后重试", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	switch status {
	case "active", "finished", "untapped":
	default:
		status = "all"
	}
	verdict := strings.TrimSpace(q.Get("verdict"))
	switch verdict {
	case "target_met", "below_target", "loss", "breakeven", "profitable", "recovering", "no_cost", "untapped":
	default:
		verdict = "all"
	}

	cfg := s.currentPricing()
	filtered := make([]Keg, 0, len(allKegs))
	for _, k := range allKegs {
		switch status {
		case "active":
			if k.TappedAt == nil || k.FinishedAt != nil {
				continue
			}
		case "finished":
			if k.FinishedAt == nil {
				continue
			}
		case "untapped":
			if k.TappedAt != nil || k.FinishedAt != nil {
				continue
			}
		}
		if verdict != "all" && k.EconomicsWithConfig(cfg).Verdict != verdict {
			continue
		}
		filtered = append(filtered, k)
	}

	totalPages := max((len(filtered)+kegPageSize-1)/kegPageSize, 1)
	page, _ := strconv.Atoi(strings.TrimSpace(q.Get("page")))
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * kegPageSize
	end := min(start+kegPageSize, len(filtered))
	paged := filtered[start:end]
	var prevURL, nextURL string
	if page > 1 {
		prevURL = kegListURL(status, verdict, page-1)
	}
	if page < totalPages {
		nextURL = kegListURL(status, verdict, page+1)
	}

	s.renderer.Render(w, r, "admin/kegs.html", &KegListPage{
		Layout: ui.Layout{
			Title: "桶装盈亏",
			Nav:   "kegs",
		},
		Kegs:       paged,
		KegSummary: SummarizeKegsWithConfig(filtered, cfg),
		Status:     status,
		Verdict:    verdict,
		Pricing:    cfg,
		Page:       page,
		TotalPages: totalPages,
		PrevURL:    prevURL,
		NextURL:    nextURL,
	})
}

func (s *Service) KegEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := GetKeg(r.Context(), s.pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("keg edit (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	beers, err := catalog.ListBeers(r.Context(), s.pool, false)
	if err != nil {
		log.Printf("keg edit list beers (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/keg_form.html", &KegFormPage{
		Layout: ui.Layout{Title: "编辑酒桶", Nav: "kegs"},
		Keg:    k,
		Beers:  beers,
	})
}

func (s *Service) KegSave(w http.ResponseWriter, r *http.Request) {
	capPtr, capErr := platform.ParseStrictInt(r.FormValue("capacity_ml"), "桶容量", 1, math.MaxInt32)
	costCents, costErr := platform.MoneyCents(r.FormValue("cost"))
	placedAt, placedErr := parseOptionalTimeInput(r.FormValue("placed_at"), "入库时间")
	tappedAt, tappedErr := parseOptionalTimeInput(r.FormValue("tapped_at"), "开桶时间")
	finishedAt, finishedErr := parseOptionalTimeInput(r.FormValue("finished_at"), "结桶时间")
	k := Keg{
		BeerID:     int64(platform.IntValue(r.FormValue("beer_id"))),
		KegNo:      strings.TrimSpace(r.FormValue("keg_no")),
		CapacityML: 0,
		CostCents:  costCents,
		PlacedAt:   placedAt,
		TappedAt:   tappedAt,
		FinishedAt: finishedAt,
		Note:       strings.TrimSpace(r.FormValue("note")),
	}
	if capPtr != nil {
		k.CapacityML = *capPtr
	}
	if id := r.FormValue("id"); id != "" {
		k.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	renderFail := func(status int, msg string) {
		beers, _ := catalog.ListBeers(r.Context(), s.pool, false)
		title := "新建酒桶"
		if k.ID > 0 {
			title = "编辑酒桶"
		}
		s.renderer.RenderStatus(w, r, status, "admin/keg_form.html", &KegFormPage{
			Layout: ui.Layout{Title: title, Nav: "kegs", Err: msg},
			Keg:    k,
			Beers:  beers,
		})
	}
	if capErr != nil {
		renderFail(http.StatusBadRequest, capErr.Error())
		return
	}
	if k.CapacityML <= 0 {
		renderFail(http.StatusBadRequest, "桶容量要大于 0")
		return
	}
	if costErr != nil {
		renderFail(http.StatusBadRequest, costErr.Error())
		return
	}
	if placedErr != nil {
		renderFail(http.StatusBadRequest, placedErr.Error())
		return
	}
	if tappedErr != nil {
		renderFail(http.StatusBadRequest, tappedErr.Error())
		return
	}
	if finishedErr != nil {
		renderFail(http.StatusBadRequest, finishedErr.Error())
		return
	}
	res, err := SaveKegResultFn(r.Context(), s.pool, k)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			renderFail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("keg save (%d): %v", k.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	subject := platform.BeerSubject(res.Brewery, res.BeerName)
	switch {
	case res.OpKind == platform.OpTap:
		s.publish(r, event.EventScreenChanged, subject, map[string]any{"action": "keg.tapped", "beer_id": res.BeerID, "keg_id": res.KegID})
		s.publish(r, event.EventBeerTapped, subject, map[string]any{"beer_id": res.BeerID, "keg_id": res.KegID, "opened_new": true})
	case res.OpKind == platform.OpUntap:
		s.publish(r, event.EventScreenChanged, subject, map[string]any{"action": "keg.untapped", "beer_id": res.BeerID, "keg_id": res.KegID})
		payload := map[string]any{"beer_id": res.BeerID, "keg_id": res.KegID}
		if savedKeg, err := GetKeg(r.Context(), s.pool, res.KegID); err != nil {
			log.Printf("keg save finished load (%d): %v", res.KegID, err)
		} else {
			econ := savedKeg.EconomicsWithConfig(s.currentPricing())
			payload = map[string]any{
				"keg_id":        savedKeg.ID,
				"beer_id":       savedKeg.BeerID,
				"revenue_cents": savedKeg.RevenueCents,
				"cost_cents":    savedKeg.CostCents,
				"profit_cents":  savedKeg.ProfitCents(),
				"verdict":       econ.Verdict,
				"yield_pct":     econ.ActualYieldPct,
			}
		}
		s.publish(r, event.EventKegFinished, subject, payload)
	case res.ScreenChanged:
		s.publish(r, event.EventScreenChanged, subject, map[string]any{"action": "keg.screen_off", "beer_id": res.BeerID, "keg_id": res.KegID})
	}
	platform.RedirectMsg(w, r, "/admin/kegs/"+strconv.FormatInt(res.KegID, 10), "已保存")
}

func (s *Service) KegDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := GetKeg(r.Context(), s.pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("keg detail (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	pours, err := ListPours(r.Context(), s.pool, id)
	if err != nil {
		log.Printf("keg detail list pours (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	beer, err := catalog.GetBeer(r.Context(), s.pool, k.BeerID)
	if err != nil {
		log.Printf("keg detail get beer (%d): %v", k.BeerID, err)
	}
	cfg := s.currentPricing()
	s.renderer.Render(w, r, "admin/keg.html", &KegDetailPage{
		Layout: ui.Layout{
			Title: k.Title(),
			Nav:   "kegs",
		},
		Keg:        k,
		Beer:       beer,
		KegPricing: k.CupPricingWithConfig(beer, cfg),
		Pours:      pours,
		Pricing:    cfg,
	})
}

func (s *Service) PourAdd(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := "/admin/kegs/" + strconv.FormatInt(id, 10)
	cupsPtr, cupsErr := platform.ParseStrictInt(r.FormValue("cups"), "杯数", 1, 1000)
	mlPtr, mlErr := platform.ParseStrictInt(r.FormValue("ml"), "毫升", 0, 1000000)
	amountCents, amtErr := platform.MoneyCents(r.FormValue("amount"))
	switch {
	case cupsErr != nil || cupsPtr == nil:
		msg := "杯数格式不正确"
		if cupsErr != nil {
			msg = cupsErr.Error()
		}
		platform.RedirectErr(w, r, back, msg)
		return
	case mlErr != nil:
		platform.RedirectErr(w, r, back, mlErr.Error())
		return
	case amtErr != nil:
		platform.RedirectErr(w, r, back, amtErr.Error())
		return
	}
	ml := 0
	if mlPtr != nil {
		ml = *mlPtr
	}
	if err := AddPour(r.Context(), s.pool, id, *cupsPtr, ml, amountCents); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		log.Printf("pour add (%d): %v", id, err)
		platform.RedirectErr(w, r, back, "记售出失败，请稍后重试")
		return
	}
	platform.RedirectMsg(w, r, back, "已记一笔")
}

func (s *Service) PourDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kegID := int64(platform.IntValue(r.FormValue("keg_id")))
	back := "/admin/kegs"
	if kegID > 0 {
		back = "/admin/kegs/" + strconv.FormatInt(kegID, 10)
	}
	if err := DeletePour(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		log.Printf("pour delete (%d): %v", id, err)
		platform.RedirectErr(w, r, back, "删除失败，请稍后重试")
		return
	}
	platform.RedirectMsg(w, r, back, "已删除该笔")
}

func (s *Service) KegDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteKeg(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/kegs/"+strconv.FormatInt(id, 10), msg)
			return
		}
		log.Printf("keg delete (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/kegs/"+strconv.FormatInt(id, 10), "删除失败，请稍后重试")
		return
	}
	s.publish(r, event.EventScreenChanged, "", map[string]any{"action": "keg.deleted", "keg_id": id})
	platform.RedirectMsg(w, r, "/admin/beers", "已删除酒桶记录")
}
