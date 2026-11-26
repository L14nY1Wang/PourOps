package inventory

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"pourops/internal/catalog"
	"pourops/internal/event"
	"pourops/internal/platform"
	"pourops/internal/settings"
	"pourops/internal/ui"
)

// Service 管理酒单、酒桶进销存、桶装盈亏与零售库存业务，并在事务提交后发布领域事件。
type Service struct {
	pool     *pgxpool.Pool
	renderer *ui.Renderer
	settings *settings.SettingsStore
	bus      *event.EventBus
	posters  *catalog.PosterStore
}

func NewService(pool *pgxpool.Pool, renderer *ui.Renderer, cfg *settings.SettingsStore, bus *event.EventBus, posters *catalog.PosterStore) *Service {
	return &Service{
		pool:     pool,
		renderer: renderer,
		settings: cfg,
		bus:      bus,
		posters:  posters,
	}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, guards ui.Guards) {
	mux.HandleFunc("GET /admin/beers", guards.Auth(s.BeerList))
	mux.HandleFunc("GET /admin/beers/purchase", guards.Auth(s.PurchasePage))
	mux.HandleFunc("POST /admin/beers/purchase", guards.Auth(s.PurchaseBeer))
	mux.HandleFunc("GET /admin/beers/{id}/edit", guards.Auth(s.EditBeer))
	mux.HandleFunc("POST /admin/beers", guards.Auth(s.SaveBeer))
	mux.HandleFunc("POST /admin/beers/{id}/tap", guards.Auth(s.TapBeerHandler))
	mux.HandleFunc("POST /admin/beers/{id}/untap", guards.Auth(s.UntapBeerHandler))
	mux.HandleFunc("POST /admin/beers/{id}/toggle", guards.Auth(s.ToggleBeerHandler))
	mux.HandleFunc("POST /admin/beers/{id}/delete", guards.Auth(s.DeleteBeerHandler))

	mux.HandleFunc("GET /admin/kegs", guards.Auth(s.KegList))
	mux.HandleFunc("GET /admin/kegs/{id}", guards.Auth(s.KegDetail))
	mux.HandleFunc("GET /admin/kegs/{id}/edit", guards.Auth(s.KegEdit))
	mux.HandleFunc("POST /admin/kegs", guards.Auth(s.KegSave))
	mux.HandleFunc("POST /admin/kegs/{id}/pours", guards.Auth(s.PourAdd))
	mux.HandleFunc("POST /admin/kegs/{id}/delete", guards.Boss(s.KegDelete))
	mux.HandleFunc("POST /admin/pours/{id}/delete", guards.Auth(s.PourDelete))

	mux.HandleFunc("GET /admin/retail", guards.Auth(s.RetailList))
	mux.HandleFunc("GET /admin/retail/new", guards.Auth(s.RetailNew))
	mux.HandleFunc("GET /admin/retail/{id}/edit", guards.Auth(s.RetailEdit))
	mux.HandleFunc("POST /admin/retail", guards.Auth(s.RetailSave))
	mux.HandleFunc("POST /admin/retail/{id}/stock", guards.Auth(s.RetailStock))
	mux.HandleFunc("POST /admin/retail/{id}/toggle", guards.Auth(s.RetailToggle))
	mux.HandleFunc("POST /admin/retail/{id}/delete", guards.Auth(s.RetailDelete))
}

func (s *Service) currentPricing() settings.PricingConfig {
	if s.settings != nil {
		return s.settings.Pricing()
	}
	return settings.DefaultPricingConfig()
}

func (s *Service) currentCategories() []string {
	if s.settings != nil {
		return s.settings.BeerCategories()
	}
	return platform.DefaultBeerCategories
}

func (s *Service) publish(r *http.Request, typ event.EventType, subject string, payload any) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(r.Context(), typ, subject, payload)
}
