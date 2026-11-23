package report

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/inventory"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type DashboardPage struct {
	ui.Layout
	Overview     Overview
	Kegs         []inventory.Keg
	Sales        SalesInsights
	POS          POSInsights
	ReportPeriod string
	ReportStart  string
	ReportEnd    string
	Pricing      settings.PricingConfig
}

func (p DashboardPage) PricingCfg() settings.PricingConfig {
	return p.Pricing.Normalized()
}

type RecordsViewPage struct {
	ui.Layout
	Records *RecordsPage
}

// Service 提供经营概览（销售、POS 实收、库存快照）以及销售订单、营运流水只读审计页面。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	settings *settings.SettingsStore
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, cfg *settings.SettingsStore) *Service {
	return &Service{
		pool:     pool,
		renderer: renderer,
		settings: cfg,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/{$}", guards.Auth(s.Dashboard))
	mux.HandleFunc("GET /admin/records", guards.Auth(s.RecordsOrders))
	mux.HandleFunc("GET /admin/records/orders/{id}", guards.Auth(s.RecordsOrderDetail))
	mux.HandleFunc("GET /admin/records/operations", guards.Auth(s.RecordsOperations))
}

func (s *Service) currentPricing() settings.PricingConfig {
	if s.settings != nil {
		return s.settings.Pricing()
	}
	return settings.DefaultPricingConfig()
}

func (s *Service) Dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	down := func(err error) {
		log.Printf("dashboard: %v", err)
		http.Error(w, "暂时无法读取经营概览", http.StatusInternalServerError)
	}
	o, err := LoadOverview(ctx, s.pool)
	if err != nil {
		down(err)
		return
	}
	kegs, err := inventory.ListKegs(ctx, s.pool, true)
	if err != nil {
		down(err)
		return
	}
	period, start, end, problem := ReportDates(time.Now(), q.Get("period"), q.Get("start"), q.Get("end"))
	data := &DashboardPage{
		Layout: ui.Layout{
			Title: "概览",
			Nav:   "dashboard",
			Msg:   q.Get("msg"),
			Err:   problem,
		},
		Overview:     o,
		Kegs:         kegs,
		ReportPeriod: period,
		Pricing:      s.currentPricing(),
	}
	if problem != "" {
		data.ReportStart, data.ReportEnd = q.Get("start"), q.Get("end")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		s.renderer.Render(w, r, "admin/dashboard.html", data)
		return
	}
	data.ReportStart = start.Format(time.DateOnly)
	data.ReportEnd = end.AddDate(0, 0, -1).Format(time.DateOnly)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		down(err)
		return
	}
	defer tx.Rollback(ctx)
	if data.Sales, err = LoadSalesInsights(ctx, tx, start, end); err != nil {
		down(err)
		return
	}
	if data.POS, err = LoadPOSInsights(ctx, tx, start, end); err != nil {
		down(err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		down(err)
		return
	}
	s.renderer.Render(w, r, "admin/dashboard.html", data)
}

func (s *Service) RecordsOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if !validOrderStatus(status) {
		status = ""
	}
	page := recordPageParam(r)
	orders, hasNext, err := ListRecordOrders(r.Context(), s.pool, status, page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	rec := &RecordsPage{
		Mode: "orders", Orders: orders, Status: status, Statuses: RecordOrderStatuses, Page: page,
	}
	if page > 1 {
		rec.PrevURL = recordURL("/admin/records", q, page-1)
	}
	if hasNext {
		rec.NextURL = recordURL("/admin/records", q, page+1)
	}
	s.renderer.Render(w, r, "admin/records.html", &RecordsViewPage{
		Layout:  ui.Layout{Title: "销售订单", Nav: "orders"},
		Records: rec,
	})
}

func (s *Service) RecordsOrderDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "找不到这张单", http.StatusNotFound)
		return
	}
	o, checkouts, unpaid, unlinked, err := LoadRecordDetail(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这张单", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderer.Render(w, r, "admin/records.html", &RecordsViewPage{
		Layout: ui.Layout{Title: "订单 #" + strconv.FormatInt(id, 10), Nav: "orders"},
		Records: &RecordsPage{
			Mode: "order", Order: &o, Checkouts: checkouts, Unpaid: unpaid, Unlinked: unlinked,
		},
	})
}

func (s *Service) RecordsOperations(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if !validOperationKind(kind) {
		kind = ""
	}
	page := recordPageParam(r)
	events, hasNext, err := ListLedgerEvents(r.Context(), s.pool, kind, page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	rec := &RecordsPage{
		Mode: "operations", Events: events, Kind: kind, Kinds: RecordKindOptions, Page: page,
	}
	if page > 1 {
		rec.PrevURL = recordURL("/admin/records/operations", q, page-1)
	}
	if hasNext {
		rec.NextURL = recordURL("/admin/records/operations", q, page+1)
	}
	s.renderer.Render(w, r, "admin/records.html", &RecordsViewPage{
		Layout:  ui.Layout{Title: "营运流水", Nav: "operations"},
		Records: rec,
	})
}
