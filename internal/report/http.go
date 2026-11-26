package report

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

type DashboardPage struct {
	ui.Layout
	Overview      Overview
	Sales         SalesInsights
	POS           POSInsights
	Loyalty       LoyaltyInsights
	InvalidPeriod bool
	ReportPeriod  string
	ReportStart   string
	ReportEnd     string
}

type AnalyticsPage struct {
	ui.Layout
	Sales         SalesInsights
	Audience      AudienceInsights
	InvalidPeriod bool
	ReportPeriod  string
	ReportStart   string
	ReportEnd     string
}

type RecordsViewPage struct {
	ui.Layout
	Records *RecordsPage
}

// Service 提供经营概览、数据分析（销售走势与营业日统计）以及销售订单、营运流水只读审计页面。
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
	mux.HandleFunc("GET /admin/analytics", guards.Auth(s.Analytics))
	mux.HandleFunc("GET /admin/records", guards.Auth(s.RecordsOrders))
	mux.HandleFunc("GET /admin/records/orders/{id}", guards.Auth(s.RecordsOrderDetail))
	mux.HandleFunc("GET /admin/records/operations", guards.Auth(s.RecordsOperations))
}

type reportWindow struct {
	period    string
	start     time.Time
	end       time.Time
	startText string
	endText   string
	problem   string
}

func parseReportWindow(r *http.Request) reportWindow {
	q := r.URL.Query()
	period, start, end, problem := ReportDates(time.Now(), q.Get("period"), q.Get("start"), q.Get("end"))
	w := reportWindow{
		period:  period,
		start:   start,
		end:     end,
		problem: problem,
	}
	if problem != "" {
		w.startText, w.endText = q.Get("start"), q.Get("end")
		return w
	}
	w.startText = start.Format(time.DateOnly)
	w.endText = end.AddDate(0, 0, -1).Format(time.DateOnly)
	return w
}

func (s *Service) Dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	down := func(err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("dashboard: %v", err)
		http.Error(w, "暂时无法读取经营概览", http.StatusInternalServerError)
	}
	win := parseReportWindow(r)
	data := &DashboardPage{
		Layout: ui.Layout{
			Title: "概览",
			Nav:   "dashboard",
			Err:   win.problem,
		},
		InvalidPeriod: win.problem != "",
		ReportPeriod:  win.period,
		ReportStart:   win.startText,
		ReportEnd:     win.endText,
	}
	if win.problem != "" {
		var err error
		if data.Overview, err = LoadOverview(ctx, s.pool); err != nil {
			down(err)
			return
		}
		s.renderer.RenderStatus(w, r, http.StatusBadRequest, "admin/dashboard.html", data)
		return
	}
	// 概览、销售与 POS 共用一个 RepeatableRead 只读事务：页面上的数字必须同一快照，
	// 否则并发结账时库存和当日营业额会互相矛盾。
	tx, err := beginReadOnlyTimeoutTx(ctx, s.pool)
	if err != nil {
		down(err)
		return
	}
	defer tx.Rollback(ctx)
	if data.Overview, err = LoadOverviewTx(ctx, tx); err != nil {
		down(err)
		return
	}
	if data.Sales, err = LoadSalesInsights(ctx, tx, win.start, win.end); err != nil {
		down(err)
		return
	}
	if data.POS, err = LoadPOSInsights(ctx, tx, win.start, win.end); err != nil {
		down(err)
		return
	}
	if data.Loyalty, err = LoadLoyaltyInsights(ctx, tx, win.start, win.end, time.Now()); err != nil {
		down(err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		down(err)
		return
	}
	s.renderer.Render(w, r, "admin/dashboard.html", data)
}

func (s *Service) currentCategories() []string {
	if s.settings != nil {
		return s.settings.BeerCategories()
	}
	return platform.DefaultBeerCategories
}

func (s *Service) Analytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	down := func(err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("analytics: %v", err)
		http.Error(w, "暂时无法读取数据分析", http.StatusInternalServerError)
	}
	win := parseReportWindow(r)
	data := &AnalyticsPage{
		Layout: ui.Layout{
			Title: "数据分析",
			Nav:   "analytics",
			Err:   win.problem,
		},
		InvalidPeriod: win.problem != "",
		ReportPeriod:  win.period,
		ReportStart:   win.startText,
		ReportEnd:     win.endText,
	}
	if win.problem != "" {
		s.renderer.RenderStatus(w, r, http.StatusBadRequest, "admin/analytics.html", data)
		return
	}
	tx, err := beginReadOnlyTimeoutTx(ctx, s.pool)
	if err != nil {
		down(err)
		return
	}
	defer tx.Rollback(ctx)
	if data.Sales, err = LoadSalesInsights(ctx, tx, win.start, win.end); err != nil {
		down(err)
		return
	}
	if data.Audience, err = LoadAudienceInsights(ctx, tx, win.start, win.end, s.currentCategories()); err != nil {
		down(err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		down(err)
		return
	}
	s.renderer.Render(w, r, "admin/analytics.html", data)
}

func (s *Service) RecordsOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if !validOrderStatus(status) {
		status = ""
	}
	beforeID := parseOrderBeforeCursor(r.URL.Query().Get("before"))
	orders, hasNext, err := ListRecordOrdersCursor(r.Context(), s.pool, status, beforeID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("records orders: %v", err)
			http.Error(w, "暂时无法读取销售订单", http.StatusInternalServerError)
		}
		return
	}
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	rec := &RecordsPage{
		Mode: "orders", Orders: orders, Status: status, Statuses: RecordOrderStatuses,
	}
	if beforeID > 0 {
		rec.PrevURL = recordCursorURL("/admin/records", q, "")
	}
	if hasNext && len(orders) > 0 {
		rec.NextURL = recordCursorURL("/admin/records", q, strconv.FormatInt(orders[len(orders)-1].ID, 10))
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
		if !errors.Is(err, context.Canceled) {
			log.Printf("records order detail (%d): %v", id, err)
			http.Error(w, "暂时无法读取订单详情", http.StatusInternalServerError)
		}
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
	beforeAt, beforeID := parseEventCursor(r.URL.Query().Get("before"))
	events, hasNext, err := ListLedgerEventsCursor(r.Context(), s.pool, kind, beforeAt, beforeID)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("records operations: %v", err)
			http.Error(w, "暂时无法读取营运流水", http.StatusInternalServerError)
		}
		return
	}
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	rec := &RecordsPage{
		Mode: "operations", Events: events, Kind: kind, Kinds: RecordKindOptions,
	}
	if beforeAt != nil {
		rec.PrevURL = recordCursorURL("/admin/records/operations", q, "")
	}
	if hasNext && len(events) > 0 {
		last := events[len(events)-1]
		rec.NextURL = recordCursorURL("/admin/records/operations", q, formatEventCursor(last.OccurredAt, last.ID))
	}
	s.renderer.Render(w, r, "admin/records.html", &RecordsViewPage{
		Layout:  ui.Layout{Title: "营运流水", Nav: "operations"},
		Records: rec,
	})
}
