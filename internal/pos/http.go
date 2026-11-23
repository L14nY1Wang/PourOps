package pos

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/event"
	"pourops/internal/inventory"
	"pourops/internal/platform"
	"pourops/internal/ui"
)

type PosPage struct {
	ui.Layout
	Spots      []Spot
	TableCount int
	PosBeers   []PosBeer
	PosItems   []inventory.RetailItem
	Order      *OrderView
	PosOrderID int64
}

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
	mux.HandleFunc("POST /pos/bar/add", guards.Any(s.PosAddBar))
	mux.HandleFunc("POST /pos/table/add", guards.Any(s.PosAddTable))
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
		Layout: ui.Layout{
			Err: r.URL.Query().Get("err"),
			Msg: r.URL.Query().Get("msg"),
		},
		PosBeers: beers,
		PosItems: items,
	}, nil
}

func (s *Service) PosSpots(w http.ResponseWriter, r *http.Request) {
	data, err := s.posMenu(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	spots, err := ListSpots(r.Context(), s.pool)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Title = "点餐"
	data.Spots = spots
	for _, sp := range spots {
		if sp.Kind == "table" {
			data.TableCount++
		}
	}
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddBar(w http.ResponseWriter, r *http.Request) {
	if err := AddSpot(r.Context(), s.pool, "bar_count"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/pos/", http.StatusSeeOther)
}

func (s *Service) PosAddTable(w http.ResponseWriter, r *http.Request) {
	if err := AddSpot(r.Context(), s.pool, "table_count"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/pos/", http.StatusSeeOther)
}

func (s *Service) PosOpen(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("spot")
	if !strings.HasPrefix(key, "bar-") && !strings.HasPrefix(key, "table-") {
		http.Redirect(w, r, "/pos/?msg="+platform.URLMsg("没有这个台位"), http.StatusSeeOther)
		return
	}
	id, err := EnsureOrder(r.Context(), s.pool, key)
	if err != nil {
		http.Redirect(w, r, "/pos/?msg="+platform.URLMsg(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/pos/order/%d", id), http.StatusSeeOther)
}

func (s *Service) PosOrder(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := LoadOrder(r.Context(), s.pool, id)
	if err != nil {
		http.Error(w, "找不到这张账单", http.StatusNotFound)
		return
	}
	data, err := s.posMenu(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Title = o.Label
	data.Order = &o
	if !o.Closed {
		data.PosOrderID = o.ID
	}
	s.renderer.RenderShell(w, r, "pos/pos.html", "pos", data)
}

func (s *Service) PosAddItem(w http.ResponseWriter, r *http.Request) {
	orderID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := fmt.Sprintf("/pos/order/%d", orderID)
	refID, _ := strconv.ParseInt(r.FormValue("ref"), 10, 64)

	switch r.FormValue("kind") {
	case "beer":
		ml, _ := strconv.Atoi(r.FormValue("ml"))
		var name string
		var price int
		err := s.pool.QueryRow(r.Context(), `
			select b.brewery || ' · ' || b.name,
				case $2::int when 150 then coalesce(b.price_150, 0)
					when 330 then coalesce(b.price_330, 0)
					when 500 then coalesce(b.price_500, 0)
					else 0 end
			from kegs k join beers b on b.id = k.beer_id
			where k.id=$1 and b.on_screen and k.tapped_at is not null and k.finished_at is null`, refID, ml).Scan(&name, &price)
		if err != nil {
			http.Redirect(w, r, back+"?err="+platform.URLMsg("这桶酒不在售"), http.StatusSeeOther)
			return
		}
		if price <= 0 {
			http.Redirect(w, r, back+"?err="+platform.URLMsg("这桶酒没开这个杯型"), http.StatusSeeOther)
			return
		}
		if err := AddOrderItem(r.Context(), s.pool, orderID, "beer", refID, name, price, ml); err != nil {
			http.Redirect(w, r, back+"?err="+platform.URLMsg(err.Error()), http.StatusSeeOther)
			return
		}
	case "retail":
		it, err := inventory.GetRetail(r.Context(), s.pool, refID)
		if err != nil || !it.Active || it.Stock < 1 {
			http.Redirect(w, r, back+"?err="+platform.URLMsg("这件商品不能卖了"), http.StatusSeeOther)
			return
		}
		name := it.Name
		if it.Spec != "" {
			name += " " + it.Spec
		}
		if err := AddOrderItem(r.Context(), s.pool, orderID, "retail", refID, name, it.PriceCents, 0); err != nil {
			http.Redirect(w, r, back+"?err="+platform.URLMsg(err.Error()), http.StatusSeeOther)
			return
		}
	default:
		http.Redirect(w, r, back+"?err="+platform.URLMsg("不认识的商品"), http.StatusSeeOther)
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
		http.Redirect(w, r, fmt.Sprintf("/pos/order/%d?err=%s", orderID, platform.URLMsg(err.Error())), http.StatusSeeOther)
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
	res, err := PayItemsWithGateway(r.Context(), s.pool, s.gateway, orderID, ids)
	if err != nil {
		msg := err.Error()
		if um, ok := platform.UserErrorMessage(err); ok {
			msg = um
		}
		http.Redirect(w, r, fmt.Sprintf("/pos/order/%d?err=%s", orderID, platform.URLMsg(msg)), http.StatusSeeOther)
		return
	}

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
		if err := s.printer.Print(r.Context(), receipt); err != nil {
			log.Printf("pos receipt print (%s) payment %d failed: %v", s.printer.Name(), res.PaymentID, err)
		}
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

	http.Redirect(w, r, fmt.Sprintf("/pos/pay/%d", res.PaymentID), http.StatusSeeOther)
}

func (s *Service) PosPayPage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := LoadPayment(r.Context(), s.pool, id)
	if err != nil {
		http.Error(w, "找不到这笔账", http.StatusNotFound)
		return
	}
	s.renderer.RenderShell(w, r, "pos/pos_pay.html", "pos_pay", &PayPage{
		Layout: ui.Layout{Title: "收款"},
		Pay:    &p,
	})
}
