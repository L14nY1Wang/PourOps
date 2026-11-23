package inventory

import (
	"net/http"
	"strconv"
	"strings"

	"pourops/internal/catalog"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type KegListPage struct {
	ui.Layout
	Kegs       []Keg
	KegSummary KegSummary
	Status     string
	Verdict    string
	Pricing    settings.PricingConfig
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
	return p.Keg.EconomicsWithConfig(&p.Beer, p.PricingCfg())
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		if verdict != "all" && k.EconomicsWithConfig(nil, cfg).Verdict != verdict {
			continue
		}
		filtered = append(filtered, k)
	}

	s.renderer.Render(w, r, "admin/kegs.html", &KegListPage{
		Layout: ui.Layout{
			Title: "桶装盈亏",
			Nav:   "kegs",
			Msg:   q.Get("msg"),
			Err:   q.Get("err"),
		},
		Kegs:       filtered,
		KegSummary: SummarizeKegsWithConfig(filtered, cfg),
		Status:     status,
		Verdict:    verdict,
		Pricing:    cfg,
	})
}

func (s *Service) KegEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := GetKeg(r.Context(), s.pool, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	beers, err := catalog.ListBeers(r.Context(), s.pool, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/keg_form.html", &KegFormPage{
		Layout: ui.Layout{Title: "编辑酒桶", Nav: "kegs"},
		Keg:    k,
		Beers:  beers,
	})
}

func (s *Service) KegSave(w http.ResponseWriter, r *http.Request) {
	k := Keg{
		BeerID:     int64(platform.IntValue(r.FormValue("beer_id"))),
		KegNo:      strings.TrimSpace(r.FormValue("keg_no")),
		CapacityML: platform.IntValue(r.FormValue("capacity_ml")),
		CostCents:  platform.PriceCents(r.FormValue("cost")),
		PlacedAt:   platform.ParseLocalTime(r.FormValue("placed_at")),
		TappedAt:   platform.ParseLocalTime(r.FormValue("tapped_at")),
		FinishedAt: platform.ParseLocalTime(r.FormValue("finished_at")),
		Note:       strings.TrimSpace(r.FormValue("note")),
	}
	if id := r.FormValue("id"); id != "" {
		k.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	id, err := SaveKeg(r.Context(), s.pool, k)
	if err != nil {
		beers, _ := catalog.ListBeers(r.Context(), s.pool, false)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		s.renderer.Render(w, r, "admin/keg_form.html", &KegFormPage{
			Layout: ui.Layout{Title: "新建酒桶", Nav: "kegs", Err: err.Error()},
			Keg:    k,
			Beers:  beers,
		})
		return
	}
	http.Redirect(w, r, "/admin/kegs/"+strconv.FormatInt(id, 10)+"?msg="+platform.URLMsg("已保存"), http.StatusSeeOther)
}

func (s *Service) KegDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := GetKeg(r.Context(), s.pool, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pours, err := ListPours(r.Context(), s.pool, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	beer, _ := catalog.GetBeer(r.Context(), s.pool, k.BeerID)
	cfg := s.currentPricing()
	s.renderer.Render(w, r, "admin/keg.html", &KegDetailPage{
		Layout: ui.Layout{
			Title: k.Title(),
			Nav:   "kegs",
			Msg:   r.URL.Query().Get("msg"),
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
	err := AddPour(r.Context(), s.pool, id,
		platform.IntValue(r.FormValue("cups")), platform.IntValue(r.FormValue("ml")), platform.PriceCents(r.FormValue("amount")))
	if err != nil {
		http.Redirect(w, r, "/admin/kegs/"+strconv.FormatInt(id, 10)+"?msg="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/kegs/"+strconv.FormatInt(id, 10)+"?msg="+platform.URLMsg("已记一笔"), http.StatusSeeOther)
}

func (s *Service) PourDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kegID := int64(platform.IntValue(r.FormValue("keg_id")))
	if err := DeletePour(r.Context(), s.pool, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/kegs/"+strconv.FormatInt(kegID, 10)+"?msg="+platform.URLMsg("已删除该笔"), http.StatusSeeOther)
}

func (s *Service) KegDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteKeg(r.Context(), s.pool, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/beers?msg="+platform.URLMsg("已删除酒桶记录"), http.StatusSeeOther)
}
