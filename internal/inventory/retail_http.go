package inventory

import (
	"net/http"
	"strconv"
	"strings"

	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/ui"
)

type RetailListPage struct {
	ui.Layout
	Retail     []RetailItem
	Stats      RetailStats
	RetailCats []string
	Q          string
	Cat        string
	LowOnly    bool
	ActiveOnly bool
}

type RetailFormPage struct {
	ui.Layout
	Item       RetailItem
	RetailCats []string
	Moves      []StockMove
}

func (s *Service) publishLowStock(r *http.Request, low *LowStockItem) {
	if low == nil {
		return
	}
	s.publish(r, event.EventRetailLowStock, platform.RetailSubject(low.Name), map[string]any{
		"item_id": low.ItemID,
		"name":    low.Name,
		"stock":   low.Stock,
		"active":  low.Active,
	})
}

func (s *Service) RetailList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := SearchRetail(r.Context(), s.pool, q.Get("q"), q.Get("cat"), q.Get("low") != "", q.Get("active") != "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := RetailStatsSummary(r.Context(), s.pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	all, err := ListRetail(r.Context(), s.pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var value, lowCount int
	for _, it := range all {
		value += it.StockValue()
		if it.Stock <= LowStockThreshold {
			lowCount++
		}
	}
	stats.StockValue, stats.LowCount, stats.SKUs = value, lowCount, len(all)

	s.renderer.Render(w, r, "admin/retail.html", &RetailListPage{
		Layout: ui.Layout{
			Title: "零售",
			Nav:   "retail",
			Msg:   q.Get("msg"),
			Err:   q.Get("err"),
		},
		Retail:     items,
		Stats:      stats,
		RetailCats: RetailCategories,
		Q:          q.Get("q"),
		Cat:        q.Get("cat"),
		LowOnly:    q.Get("low") != "",
		ActiveOnly: q.Get("active") != "",
	})
}

func (s *Service) RetailNew(w http.ResponseWriter, r *http.Request) {
	s.renderer.Render(w, r, "admin/retail_form.html", &RetailFormPage{
		Layout:     ui.Layout{Title: "新增零售商品", Nav: "retail"},
		Item:       RetailItem{Category: "罐装", Active: true},
		RetailCats: RetailCategories,
	})
}

func (s *Service) RetailEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := GetRetail(r.Context(), s.pool, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	moves, err := ListMoves(r.Context(), s.pool, id, 12)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/retail_form.html", &RetailFormPage{
		Layout:     ui.Layout{Title: "编辑零售商品", Nav: "retail"},
		Item:       it,
		RetailCats: RetailCategories,
		Moves:      moves,
	})
}

func (s *Service) RetailSave(w http.ResponseWriter, r *http.Request) {
	it := RetailItem{
		Name:       strings.TrimSpace(r.FormValue("name")),
		Category:   r.FormValue("category"),
		Spec:       strings.TrimSpace(r.FormValue("spec")),
		PriceCents: platform.PriceCents(r.FormValue("price")),
		CostCents:  platform.PriceCents(r.FormValue("cost")),
		Stock:      platform.IntValue(r.FormValue("stock")),
		Active:     r.FormValue("active") != "",
	}
	if id := r.FormValue("id"); id != "" {
		it.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	_, low, err := SaveRetail(r.Context(), s.pool, it)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		s.renderer.Render(w, r, "admin/retail_form.html", &RetailFormPage{
			Layout:     ui.Layout{Title: "零售商品", Nav: "retail", Err: err.Error()},
			Item:       it,
			RetailCats: RetailCategories,
		})
		return
	}
	s.publishLowStock(r, low)
	http.Redirect(w, r, "/admin/retail?msg="+platform.URLMsg("已保存"), http.StatusSeeOther)
}

func (s *Service) RetailDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteRetail(r.Context(), s.pool, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/retail?msg="+platform.URLMsg("已删除"), http.StatusSeeOther)
}

func (s *Service) RetailToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := ToggleRetail(r.Context(), s.pool, id); err != nil {
		http.Redirect(w, r, "/admin/retail?msg="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/retail?msg="+platform.URLMsg("已切换上下架"), http.StatusSeeOther)
}

func (s *Service) RetailStock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	delta := platform.IntValue(r.FormValue("delta"))
	reason := "调整"
	if delta > 0 {
		reason = "入库"
	}
	low, err := AddStock(r.Context(), s.pool, id, delta, reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.publishLowStock(r, low)
	http.Redirect(w, r, "/admin/retail?msg="+platform.URLMsg("库存已更新"), http.StatusSeeOther)
}
