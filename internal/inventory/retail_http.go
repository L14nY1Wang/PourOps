package inventory

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

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
}

type RetailFormPage struct {
	ui.Layout
	Item          RetailItem
	ExpectedStock int
	RetailCats    []string
	Moves         []StockMove
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
	searchQ := strings.TrimSpace(q.Get("q"))
	cat := q.Get("cat")
	lowOnly := q.Get("low") == "1"
	activeOnly := q.Get("active") == "1"
	items, err := SearchRetail(r.Context(), s.pool, searchQ, cat, lowOnly, activeOnly)
	if err != nil {
		log.Printf("retail list search: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	stats, err := RetailStatsSummary(r.Context(), s.pool)
	if err != nil {
		log.Printf("retail list stats: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	all := items
	if searchQ != "" || cat != "" || lowOnly || activeOnly {
		all, err = ListRetail(r.Context(), s.pool)
		if err != nil {
			log.Printf("retail list all: %v", err)
			http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
			return
		}
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
		},
		Retail:     items,
		Stats:      stats,
		RetailCats: RetailCategories,
		Q:          q.Get("q"),
		Cat:        cat,
		LowOnly:    lowOnly,
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
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("retail edit (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	moves, err := ListMoves(r.Context(), s.pool, id, 12)
	if err != nil {
		log.Printf("retail edit moves (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/retail_form.html", &RetailFormPage{
		Layout:        ui.Layout{Title: "编辑零售商品", Nav: "retail"},
		Item:          it,
		ExpectedStock: it.Stock,
		RetailCats:    RetailCategories,
		Moves:         moves,
	})
}

func (s *Service) RetailSave(w http.ResponseWriter, r *http.Request) {
	priceCents, priceErr := platform.MoneyCents(r.FormValue("price"))
	costCents, costErr := platform.MoneyCents(r.FormValue("cost"))
	stockPtr, stockErr := platform.ParseStrictInt(r.FormValue("stock"), "库存", 0, math.MaxInt32)
	it := RetailItem{
		Name:       strings.TrimSpace(r.FormValue("name")),
		Category:   r.FormValue("category"),
		Spec:       strings.TrimSpace(r.FormValue("spec")),
		PriceCents: priceCents,
		CostCents:  costCents,
		Active:     r.FormValue("active") != "",
	}
	if stockPtr != nil {
		it.Stock = *stockPtr
	}
	if id := r.FormValue("id"); id != "" {
		it.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	var expectedStock *int
	if rawExp := strings.TrimSpace(r.FormValue("expected_stock")); rawExp != "" {
		if expVal, err := strconv.Atoi(rawExp); err == nil && expVal >= 0 {
			expectedStock = &expVal
		}
	}
	renderFail := func(status int, msg string) {
		title := "新增零售商品"
		if it.ID > 0 {
			title = "编辑零售商品"
		}
		expStock := it.Stock
		if expectedStock != nil {
			expStock = *expectedStock
		}
		s.renderer.RenderStatus(w, r, status, "admin/retail_form.html", &RetailFormPage{
			Layout:        ui.Layout{Title: title, Nav: "retail", Err: msg},
			Item:          it,
			ExpectedStock: expStock,
			RetailCats:    RetailCategories,
		})
	}
	switch {
	case priceErr != nil:
		renderFail(http.StatusBadRequest, priceErr.Error())
		return
	case costErr != nil:
		renderFail(http.StatusBadRequest, costErr.Error())
		return
	case stockErr != nil:
		renderFail(http.StatusBadRequest, stockErr.Error())
		return
	case stockPtr == nil:
		renderFail(http.StatusBadRequest, "库存不能为空")
		return
	}
	_, low, err := SaveRetailWithExpected(r.Context(), s.pool, it, expectedStock)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			renderFail(http.StatusBadRequest, msg)
			return
		}
		log.Printf("retail save (%d): %v", it.ID, err)
		renderFail(http.StatusInternalServerError, "保存失败，请稍后重试")
		return
	}
	s.publishLowStock(r, low)
	platform.RedirectMsg(w, r, "/admin/retail", "已保存")
}

func (s *Service) RetailDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := DeleteRetail(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/retail", msg)
			return
		}
		log.Printf("retail delete (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/retail", "删除失败，请稍后重试")
		return
	}
	platform.RedirectMsg(w, r, "/admin/retail", "已删除")
}

func (s *Service) RetailToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := ToggleRetail(r.Context(), s.pool, id); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/retail", msg)
			return
		}
		log.Printf("retail toggle (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/retail", "操作失败，请稍后重试")
		return
	}
	platform.RedirectMsg(w, r, "/admin/retail", "已切换上下架")
}

func (s *Service) RetailStock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	deltaPtr, deltaErr := platform.ParseStrictInt(r.FormValue("delta"), "库存变动数量", -100000, 100000)
	if deltaErr != nil || deltaPtr == nil || *deltaPtr == 0 {
		msg := "请填写非零整数变动数量"
		if deltaErr != nil {
			msg = deltaErr.Error()
		}
		platform.RedirectErr(w, r, "/admin/retail", msg)
		return
	}
	delta := *deltaPtr
	reason := "调整"
	if delta > 0 {
		reason = "入库"
	}
	low, err := AddStock(r.Context(), s.pool, id, delta, reason)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/admin/retail", msg)
			return
		}
		log.Printf("retail stock (%d): %v", id, err)
		platform.RedirectErr(w, r, "/admin/retail", "库存调整失败，请稍后重试")
		return
	}
	s.publishLowStock(r, low)
	platform.RedirectMsg(w, r, "/admin/retail", "库存已更新")
}
