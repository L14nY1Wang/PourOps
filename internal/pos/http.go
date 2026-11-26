package pos

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/internal/ui"
)

type PosPage struct {
	ui.Layout
	Spots      []Spot
	PosBeers   []PosBeer
	PosItems   []inventory.RetailItem
	Order      *OrderView
	PosOrderID int64
	PayToken   string
}

func (p PosPage) spotsByKind(kind string) []Spot {
	var out []Spot
	for _, sp := range p.Spots {
		if sp.Kind == kind {
			out = append(out, sp)
		}
	}
	return out
}

func (p PosPage) BarSpots() []Spot   { return p.spotsByKind("bar") }
func (p PosPage) TableSpots() []Spot { return p.spotsByKind("table") }
func (p PosPage) TempSpots() []Spot  { return p.spotsByKind("temp") }

type PayPage struct {
	ui.Layout
	Pay *Payment
}

// Service 提供台位管理、加减单品、分拆/合单结账以及编译期收款/打印扩展接口。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	bus      *event.EventBus
	gateway  PaymentGateway
	printer  ReceiptPrinter
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, bus *event.EventBus, gw PaymentGateway, printer ReceiptPrinter) *Service {
	if gw == nil {
		gw = ManualGateway{}
	}
	if printer == nil {
		printer = NoopPrinter{}
	}
	return &Service{
		pool:     pool,
		renderer: renderer,
		bus:      bus,
		gateway:  gw,
		printer:  printer,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /pos/{$}", guards.Any(s.PosSpots))
	mux.HandleFunc("POST /pos/temp/add", guards.Any(s.PosAddTemp))
	mux.HandleFunc("POST /pos/temp/remove", guards.Any(s.PosRemoveTemp))
	mux.HandleFunc("POST /pos/open", guards.Any(s.PosOpen))
	mux.HandleFunc("GET /pos/order/{id}", guards.Any(s.PosOrder))
	mux.HandleFunc("POST /pos/order/{id}/add", guards.Any(s.PosAddItem))
	mux.HandleFunc("POST /pos/order/{id}/item/{item}/less", guards.Any(s.PosLessItem))
	mux.HandleFunc("POST /pos/order/{id}/item/{item}/more", guards.Any(s.PosMoreItem))
	mux.HandleFunc("POST /pos/order/{id}/pay", guards.Any(s.PosPay))
	mux.HandleFunc("GET /pos/pay/{id}", guards.Any(s.PosPayPage))
}

func (s *Service) posMenu(r *http.Request) (*PosPage, error) {
	beers, err := ListPosBeers(r.Context(), s.pool)
	if err != nil {
		return nil, err
	}
	items, err := ListPosItems(r.Context(), s.pool)
	if err != nil {
		return nil, err
	}
	return &PosPage{
		PosBeers: beers,
		PosItems: items,
	}, nil
}

func (s *Service) PosSpots(w http.ResponseWriter, r *http.Request) {
	data, err := s.posMenu(r)
	if err != nil {
		log.Printf("pos spots menu: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	spots, err := ListSpots(r.Context(), s.pool)
	if err != nil {
		log.Printf("pos spots list: %v", err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data.Title = "点餐"
	data.Spots = spots
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddTemp(w http.ResponseWriter, r *http.Request) {
	s.tempSpotMutate(w, r, AddTempSpot)
}

func (s *Service) PosRemoveTemp(w http.ResponseWriter, r *http.Request) {
	s.tempSpotMutate(w, r, RemoveTempSpot)
}

func (s *Service) tempSpotMutate(w http.ResponseWriter, r *http.Request, fn func(context.Context, *pgxpool.Pool) error) {
	if err := fn(r.Context(), s.pool); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/pos/", msg)
			return
		}
		log.Printf("pos temp spot mutate: %v", err)
		platform.RedirectErr(w, r, "/pos/", "调整临时台位失败，请稍后重试")
		return
	}
	http.Redirect(w, r, "/pos/", http.StatusSeeOther)
}

func (s *Service) PosOpen(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("spot")
	if !strings.HasPrefix(key, "bar-") && !strings.HasPrefix(key, "table-") && !strings.HasPrefix(key, "temp-") {
		platform.RedirectErr(w, r, "/pos/", "没有这个台位")
		return
	}
	id, err := EnsureOrder(r.Context(), s.pool, key)
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, "/pos/", msg)
			return
		}
		log.Printf("pos open (%s): %v", key, err)
		platform.RedirectErr(w, r, "/pos/", "开单失败，请稍后重试")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/order/%d", id), http.StatusSeeOther)
}

func (s *Service) PosOrder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := LoadOrder(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这张账单", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("pos order (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data, err := s.posMenu(r)
	if err != nil {
		log.Printf("pos order menu (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	data.Title = o.Label
	data.Order = &o
	if !o.Closed {
		data.PosOrderID = o.ID
		data.PayToken = platform.RandomHex(16)
	}
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddItem(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/pos/order/%d", orderID)
	refID, _ := strconv.ParseInt(r.FormValue("ref"), 10, 64)

	var err error
	switch r.FormValue("kind") {
	case "beer":
		ml, _ := strconv.Atoi(r.FormValue("ml"))
		err = AddBeerOrderItem(r.Context(), s.pool, orderID, refID, ml)
	case "retail":
		err = AddRetailOrderItem(r.Context(), s.pool, orderID, refID)
	default:
		platform.RedirectErr(w, r, back, "不认识的商品")
		return
	}
	if err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, back, msg)
			return
		}
		log.Printf("pos add item (%d): %v", orderID, err)
		platform.RedirectErr(w, r, back, "加购失败，请稍后重试")
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Service) PosLessItem(w http.ResponseWriter, r *http.Request) {
	s.adjustOrderItem(w, r, LessOrderItem)
}

func (s *Service) PosMoreItem(w http.ResponseWriter, r *http.Request) {
	s.adjustOrderItem(w, r, MoreOrderItem)
}

func (s *Service) adjustOrderItem(w http.ResponseWriter, r *http.Request, op func(context.Context, *pgxpool.Pool, int64, int64) error) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	itemID, _ := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err := op(r.Context(), s.pool, orderID, itemID); err != nil {
		if msg, ok := platform.UserErrorMessage(err); ok {
			platform.RedirectErr(w, r, fmt.Sprintf("/pos/order/%d", orderID), msg)
			return
		}
		log.Printf("pos adjust item (%d/%d): %v", orderID, itemID, err)
		platform.RedirectErr(w, r, fmt.Sprintf("/pos/order/%d", orderID), "修改数量失败，请稍后重试")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/order/%d", orderID), http.StatusSeeOther)
}

func (s *Service) PosPay(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	ids := make([]int64, 0, len(r.Form["items"]))
	for _, v := range r.Form["items"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	opts := PayOptions{
		Method:         r.FormValue("method"),
		IdempotencyKey: r.FormValue("idempotency_key"),
	}
	if rawExp := strings.TrimSpace(r.FormValue("expected_total_cents")); rawExp != "" {
		if v, err := strconv.Atoi(rawExp); err == nil && v >= 0 {
			opts.ExpectedTotalCents = &v
		}
	}
	res, err := PayItemsWithOptions(r.Context(), s.pool, s.gateway, orderID, ids, opts)
	if err != nil {
		msg := "结账失败，请稍后重试"
		if um, ok := platform.UserErrorMessage(err); ok {
			msg = um
		} else {
			log.Printf("pos pay (%d): %v", orderID, err)
		}
		platform.RedirectErr(w, r, fmt.Sprintf("/pos/order/%d", orderID), msg)
		return
	}

	if !res.Replayed {
		if s.printer != nil {
			receipt := Receipt{
				PaymentID:   res.PaymentID,
				OrderID:     res.OrderID,
				SpotLabel:   res.SpotLabel,
				AmountCents: res.AmountCents,
				ItemsCount:  res.ItemsCount,
				Actor:       platform.OperationActor(r.Context()),
				Lines:       res.Lines,
			}
			printCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			if err := s.printer.Print(printCtx, receipt); err != nil {
				log.Printf("pos receipt print (%s) payment %d failed: %v", s.printer.Name(), res.PaymentID, err)
			}
			cancel()
		}
		if s.bus != nil {
			s.bus.Publish(r.Context(), event.EventOrderPaid, res.SpotLabel, map[string]any{
				"payment_id":   res.PaymentID,
				"order_id":     res.OrderID,
				"spot":         res.Spot,
				"spot_label":   res.SpotLabel,
				"amount_cents": res.AmountCents,
				"items_count":  res.ItemsCount,
				"closed":       res.Closed,
				"method":       res.Method,
				"lines":        res.Lines,
			})
			for _, low := range res.LowStock {
				s.bus.Publish(r.Context(), event.EventRetailLowStock, platform.RetailSubject(low.Name), map[string]any{
					"item_id": low.ItemID,
					"name":    low.Name,
					"stock":   low.Stock,
					"active":  low.Active,
				})
			}
		}
	}

	var notes []string
	if res.SkippedCount > 0 {
		notes = append(notes, fmt.Sprintf("已自动跳过 %d 项已结商品", res.SkippedCount))
	}
	if res.OverSoldCount > 0 {
		notes = append(notes, fmt.Sprintf("提醒：%d 款酒水累计售出已超标称桶容量", res.OverSoldCount))
	}
	if len(notes) > 0 {
		platform.RedirectMsg(w, r, fmt.Sprintf("/pos/pay/%d", res.PaymentID), strings.Join(notes, "；"))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/pay/%d", res.PaymentID), http.StatusSeeOther)
}

func (s *Service) PosPayPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := LoadPayment(r.Context(), s.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "找不到这笔账", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("pos pay page (%d): %v", id, err)
		http.Error(w, "加载失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderer.RenderShell(w, r, "pos/pos_pay.html", "pos_pay", &PayPage{
		Layout: ui.Layout{Title: "收款"},
		Pay:    &p,
	})
}
